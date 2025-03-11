package gmmu

import (
	"log"
	"reflect"

	"github.com/sarchlab/akita/v3/mem/vm"
	"github.com/sarchlab/akita/v3/sim"
	"github.com/sarchlab/akita/v3/tracing"
)

type transaction struct {
	req       *vm.TranslationReq
	page      vm.Page
	cycleLeft int
	migration *vm.PageMigrationReqToDriver
}

// gmmu is the default gmmu implementation. It is also an akita Component.
type GMMU struct {
	sim.TickingComponent

	deviceID uint64

	topPort    sim.Port
	bottomPort sim.Port

	LowModule sim.Port

	MigrationServiceProvider sim.Port

	topSender    sim.BufferedSender
	bottomSender sim.BufferedSender

	pageTable           vm.PageTable
	latency             int
	maxRequestsInFlight int

	walkingTranslations []transaction
	remoteMemReqs       map[uint64]transaction

	toRemoveFromPTW        []int
	PageAccessedByDeviceID map[uint64][]uint64

	isRecording  bool
	gpuIDList    []uint64
	isPrediction bool
}

// Tick defines how the gmmu update state each cycle
func (gmmu *GMMU) Tick(now sim.VTimeInSec) bool {
	madeProgress := false

	madeProgress = gmmu.topSender.Tick(now) || madeProgress
	madeProgress = gmmu.parseFromTop(now) || madeProgress
	madeProgress = gmmu.walkPageTable(now) || madeProgress
	madeProgress = gmmu.fetchFromBottom(now) || madeProgress

	return madeProgress
}

func (gmmu *GMMU) parseFromTop(now sim.VTimeInSec) bool {
	if len(gmmu.walkingTranslations) >= gmmu.maxRequestsInFlight {
		return false
	}

	req := gmmu.topPort.Retrieve(now)
	if req == nil {
		return false
	}

	tracing.TraceReqReceive(req, gmmu)

	switch req := req.(type) {
	case *vm.TranslationReq:
		gmmu.startWalking(req, now)
		tracing.StartTask(req.TaskID, "", gmmu, "GMMU", "TranslationReq", req)
		// fmt.Printf("%0.9f,%s,GMMUParseFromTop,%s\n",
		// 	float64(now), gmmu.TopPort.Name(), req.TaskID)

	default:
		log.Panicf("gmmu canot handle request of type %s", reflect.TypeOf(req))
	}

	return true
}

func (gmmu *GMMU) startWalking(req *vm.TranslationReq, now sim.VTimeInSec) {
	translationInPipeline := transaction{
		req:       req,
		cycleLeft: gmmu.latency,
	}

	gmmu.walkingTranslations = append(gmmu.walkingTranslations, translationInPipeline)

	if gmmu.isPrediction {
		gmmu.sendReqToBottomPort(now)

	}
}

func (gmmu *GMMU) sendReqToBottomPort(now sim.VTimeInSec) {
	lastTranslation := gmmu.walkingTranslations[len(gmmu.walkingTranslations)-1]
	req := lastTranslation.req

	page, _ := gmmu.pageTable.Find(req.PID, req.VAddr)

	if page.DeviceID == gmmu.deviceID {
		return
	}

	gmmu.processRemoteMemReq(now, len(gmmu.walkingTranslations)-1)
	// fmt.Printf("%0.9f,GMMURemotePageFault,%d,%d,%d,%d,%d\n", gmmu.Engine.CurrentTime(), gmmu.deviceID, page.DeviceID, page.PID, page.PAddr, page.VAddr)

	tmp := gmmu.walkingTranslations[:0]
	for i := 0; i < len(gmmu.walkingTranslations); i++ {
		if !gmmu.toRemove(i) {
			tmp = append(tmp, gmmu.walkingTranslations[i])
		}
	}
	gmmu.walkingTranslations = tmp
	gmmu.toRemoveFromPTW = nil
}

func (gmmu *GMMU) walkPageTable(now sim.VTimeInSec) bool {
	madeProgress := false
	for i := 0; i < len(gmmu.walkingTranslations); i++ {
		if gmmu.walkingTranslations[i].cycleLeft > 0 {
			gmmu.walkingTranslations[i].cycleLeft--
			madeProgress = true
			continue
		}
		req := gmmu.walkingTranslations[i].req

		page, _ := gmmu.pageTable.Find(req.PID, req.VAddr)

		if page.DeviceID == gmmu.deviceID {
			// if contains(gmmu.gpuIDList, page.DeviceID) {
			madeProgress = gmmu.finalizePageWalk(now, i) || madeProgress
			// fmt.Printf("%0.9f,GMMUlocalPageFault,%d,%d,%d,%d,%d\n", gmmu.Engine.CurrentTime(), gmmu.deviceID, page.DeviceID, page.PID, page.PAddr, page.VAddr)
		} else {
			madeProgress = gmmu.processRemoteMemReq(now, i) || madeProgress
			// fmt.Printf("%0.9f,GMMURemotePageFault,%d,%d,%d,%d,%d\n", gmmu.Engine.CurrentTime(), gmmu.deviceID, page.DeviceID, page.PID, page.PAddr, page.VAddr)
		}
	}

	tmp := gmmu.walkingTranslations[:0]
	for i := 0; i < len(gmmu.walkingTranslations); i++ {
		if !gmmu.toRemove(i) {
			tmp = append(tmp, gmmu.walkingTranslations[i])
		}
	}
	gmmu.walkingTranslations = tmp
	gmmu.toRemoveFromPTW = nil

	return madeProgress
}

func (gmmu *GMMU) processRemoteMemReq(now sim.VTimeInSec, walkingIndex int) bool {
	walking := gmmu.walkingTranslations[walkingIndex].req

	gmmu.remoteMemReqs[uint64(walking.VAddr)] = gmmu.walkingTranslations[walkingIndex]

	req := vm.TranslationReqBuilder{}.
		WithSendTime(now).
		WithSrc(gmmu.bottomPort).
		WithDst(gmmu.LowModule).
		WithPID(walking.PID).
		WithVAddr(walking.VAddr).
		WithDeviceID(walking.DeviceID).
		WithTaskID(walking.TaskID).
		WithOriginPort(walking.OriginPort).
		Build()

	err := gmmu.bottomPort.Send(req)

	if err != nil {
		return false
	}

	gmmu.toRemoveFromPTW = append(gmmu.toRemoveFromPTW, walkingIndex)

	return true
}

func (gmmu *GMMU) finalizePageWalk(
	now sim.VTimeInSec,
	walkingIndex int,
) bool {
	req := gmmu.walkingTranslations[walkingIndex].req
	page, _ := gmmu.pageTable.Find(req.PID, req.VAddr)

	gmmu.walkingTranslations[walkingIndex].page = page

	return gmmu.doPageWalkHit(now, walkingIndex)
}

func (gmmu *GMMU) pageNeedMigrate(walking transaction) bool {
	if walking.req.DeviceID == walking.page.DeviceID {
		return false
	}

	if !walking.page.Unified {
		return false
	}

	if walking.page.IsPinned {
		return false
	}

	return true
}

func (gmmu *GMMU) doPageWalkHit(
	now sim.VTimeInSec,
	walkingIndex int,
) bool {
	if !gmmu.topSender.CanSend(1) {
		return false
	}
	walking := gmmu.walkingTranslations[walkingIndex]

	rsp := vm.TranslationRspBuilder{}.
		WithSendTime(now).
		WithSrc(gmmu.topPort).
		WithDst(walking.req.Src).
		WithRspTo(walking.req.ID).
		WithPage(walking.page).
		WithTaskID(walking.req.TaskID).
		WithOriginPort(walking.req.OriginPort).
		Build()

	gmmu.topSender.Send(rsp)

	// fmt.Printf("%0.9f,%s,GMMULocalPageHit,%s\n",
	// 	float64(now), gmmu.topPort.Name(), rsp.TaskID)

	gmmu.toRemoveFromPTW = append(gmmu.toRemoveFromPTW, walkingIndex)

	tracing.TraceReqComplete(walking.req, gmmu)

	return true
}

func (gmmu *GMMU) sendTranlationRsp(
	now sim.VTimeInSec,
	trans transaction,
) (madeProgress bool) {
	req := trans.req
	page := trans.page

	rsp := vm.TranslationRspBuilder{}.
		WithSendTime(now).
		WithSrc(gmmu.topPort).
		WithDst(req.Src).
		WithRspTo(req.ID).
		WithPage(page).
		WithTaskID(req.TaskID).
		WithOriginPort(req.OriginPort).
		Build()
	gmmu.topSender.Send(rsp)

	// fmt.Printf("%0.9f,%s,GMMUSendTranslationRsp,%s\n",
	// 	float64(now), gmmu.topPort.Name(), rsp.TaskID)

	return true
}

func (gmmu *GMMU) toRemove(index int) bool {
	for i := 0; i < len(gmmu.toRemoveFromPTW); i++ {
		remove := gmmu.toRemoveFromPTW[i]
		if remove == index {
			return true
		}
	}
	return false
}

func (gmmu *GMMU) fetchFromBottom(now sim.VTimeInSec) bool {
	if !gmmu.topSender.CanSend(1) {
		return false
	}

	rsp := gmmu.bottomPort.Retrieve(now)
	if rsp == nil {
		return false
	}

	tracing.TraceReqReceive(rsp, gmmu)

	switch rsp := rsp.(type) {
	case *vm.TranslationRsp:
		return gmmu.handleTranslationRsp(now, rsp)
	default:
		log.Panicf("gmmu canot handle request of type %s", reflect.TypeOf(rsp))
	}

	return true
}

func (gmmu *GMMU) handleTranslationRsp(now sim.VTimeInSec, rsponse *vm.TranslationRsp) bool {
	reqTransaction := gmmu.remoteMemReqs[uint64(rsponse.Page.VAddr)]

	rsp := vm.TranslationRspBuilder{}.
		WithSendTime(now).
		WithSrc(gmmu.topPort).
		WithDst(reqTransaction.req.Src).
		WithRspTo(rsponse.ID).
		WithPage(rsponse.Page).
		WithTaskID(reqTransaction.req.TaskID).
		WithOriginPort(reqTransaction.req.OriginPort).
		Build()

	gmmu.topSender.Send(rsp)

	// fmt.Printf("%0.9f,%s,GMMUHandleTranslationRsp,%s\n",
	// 	float64(now), gmmu.topPort.Name(), rsp.TaskID)

	delete(gmmu.remoteMemReqs, uint64(rsponse.Page.VAddr))
	return true
}

// func (gmmu *GMMU) getRemoteDeviceID(vAddr uint64) uint64 {
// 	page, found := gmmu.pageTable.FindByVAddr(vAddr)
// 	if !found {
// 		panic("page not found")
// 	}

// 	return page.DeviceID
// }

func isGPUInList(gpuID uint64, gpuIDList []uint64) bool {
	for _, id := range gpuIDList {
		if id == gpuID {
			return true
		}
	}
	return false
}

func (gmmu *GMMU) checkConnectedGPUID() []int {
	var connectedGPUID []int
	for deviceID := range gmmu.PageAccessedByDeviceID {
		connectedGPUID = append(connectedGPUID, int(deviceID))
	}
	return connectedGPUID
}

func contains(list []uint64, value uint64) bool {
	for _, v := range list {
		if v == value {
			return true
		}
	}
	return false
}

func (gmmu *GMMU) GetDeviceID() uint64 {
	return gmmu.deviceID
}

func (gmmu *GMMU) GetPageID(msg *vm.TranslationReq) uint64 {
	page, _ := gmmu.pageTable.Find(msg.PID, msg.VAddr)
	return page.DeviceID
}
