package tlb_gmmu

import (
	"fmt"
	"log"
	"math"
	"reflect"

	"github.com/sarchlab/akita/v3/mem/mem"
	"github.com/sarchlab/akita/v3/mem/vm"
	"github.com/sarchlab/akita/v3/mem/vm/tlb_gmmu/internal"
	"github.com/sarchlab/akita/v3/sim"
	"github.com/sarchlab/akita/v3/tracing"
)

var nilPage = vm.Page{}

// A TLB is a cache that maintains some page information.
type GMMUTLB struct {
	*sim.TickingComponent

	topPort     sim.Port
	bottomPort  sim.Port
	OutsidePort sim.Port
	controlPort sim.Port
	IOMMUPort   sim.Port

	LowModule sim.Port

	numSets        int
	numWays        int
	pageSize       uint64
	numReqPerCycle int

	Sets []internal.Set

	mshr                mshr
	respondingMSHREntry []*mshrEntry

	isPaused  bool
	DeviceID  uint64
	pageTable vm.PageTable
	PageFider mem.PageFinder
}

// Reset sets all the entries int he TLB to be invalid
func (tlb *GMMUTLB) reset() {
	tlb.Sets = make([]internal.Set, tlb.numSets)
	for i := 0; i < tlb.numSets; i++ {
		set := internal.NewSet(tlb.numWays)
		tlb.Sets[i] = set
	}
}

// Tick defines how TLB update states at each cycle
func (tlb *GMMUTLB) Tick(now sim.VTimeInSec) bool {
	madeProgress := false

	madeProgress = tlb.performCtrlReq(now) || madeProgress

	if !tlb.isPaused {
		for i := 0; i < tlb.numReqPerCycle; i++ {
			madeProgress = tlb.respondMSHREntry(now) || madeProgress
		}

		for i := 0; i < tlb.numReqPerCycle; i++ {
			madeProgress = tlb.lookupFromTopPort(now) || madeProgress
			madeProgress = tlb.lookupFromOutsidePort(now) || madeProgress
		}

		for i := 0; i < tlb.numReqPerCycle; i++ {
			madeProgress = tlb.parseBottom(now) || madeProgress
		}
	}

	return madeProgress
}

func (tlb *GMMUTLB) respondMSHREntry(now sim.VTimeInSec) bool {
	if len(tlb.respondingMSHREntry) == 0 {
		return false
	}

	mshrEntry := tlb.respondingMSHREntry[0]
	page := mshrEntry.page
	req := mshrEntry.Requests[0]

	if req.LocalFlag {
		rspToTop := vm.TranslationRspBuilder{}.
			WithSendTime(now).
			WithSrc(tlb.topPort).
			WithDst(req.Request.Src).
			WithRspTo(req.Request.ID).
			WithPage(page).
			WithTaskID(req.Request.TaskID).
			WithOriginPort(req.Request.OriginPort).
			Build()

		// fmt.Printf("%0.9f,%s,RspToTop,%s,%d,%d,%d\n", float64(now), tlb.topPort.Name(), rspToTop.TaskID, page.VAddr, tlb.deviceID, page.DeviceID)
		err := tlb.topPort.Send(rspToTop)
		if err != nil {
			return false
		}
	}

	if req.RemoteFlag {
		rspToOutside := vm.TranslationRspBuilder{}.
			WithSendTime(now).
			WithSrc(tlb.OutsidePort).
			WithDst(req.Request.Src).
			WithRspTo(req.Request.ID).
			WithPage(page).
			WithTaskID(req.Request.TaskID).
			WithOriginPort(req.Request.OriginPort).
			Build()

		// fmt.Printf("%0.9f,%s,RspToOutside,%s,%d,%d,%d\n", float64(now), tlb.OutsidePort.Name(), rspToOutside.TaskID, page.VAddr, tlb.deviceID, page.DeviceID)
		err := tlb.OutsidePort.Send(rspToOutside)
		if err != nil {
			return false
		}
	}

	mshrEntry.Requests = mshrEntry.Requests[1:]
	if len(mshrEntry.Requests) == 0 {
		tlb.respondingMSHREntry = tlb.respondingMSHREntry[1:]
	}

	// if len(mshrEntry.Requests) == 0 {
	// 	tlb.respondingMSHREntry = nil
	// }

	tracing.TraceReqComplete(req.Request, tlb)
	return true
}

func (tlb *GMMUTLB) lookupFromTopPort(now sim.VTimeInSec) bool {
	msg := tlb.topPort.Peek()
	if msg == nil {
		return false
	}

	req := msg.(*vm.TranslationReq)
	// fmt.Printf("%0.9f,%s,FetchReqFromTop,%s,%d,%d\n", float64(now), tlb.topPort.Name(), req.TaskID, req.VAddr, tlb.deviceID)
	return tlb.processTranslation(now, req, true, false)
}

func (tlb *GMMUTLB) lookupFromOutsidePort(now sim.VTimeInSec) bool {
	msg := tlb.OutsidePort.Peek()
	if msg == nil {
		return false
	}

	switch msg := msg.(type) {
	case *vm.TranslationReq:
		return tlb.processTranslation(now, msg, false, true)
	case *vm.TranslationRsp:
		return tlb.processRsp(now, msg, false)
	default:
		panic("unexpected message type")
	}

	// fmt.Printf("%0.9f,%s,FetchReqFromOutside,%s, %d\n", float64(now), tlb.outsidePort.Name(), req.TaskID, req.VAddr)

}

func (tlb *GMMUTLB) handleTranslationHit(
	now sim.VTimeInSec,
	req *MshrRequest,
	setID, wayID int,
	page vm.Page,
) bool {

	if req.LocalFlag {
		ok := tlb.sendRspToTop(now, req.Request, page)
		// fmt.Printf(" sendRspToTop: %0.9f,%s,RspToTop,%s,%d\n", float64(now), tlb.topPort.Name(), req.Request.TaskID, page.VAddr)
		if !ok {
			return false
		}
		tlb.topPort.Retrieve(now)
	}

	if req.RemoteFlag {
		ok := tlb.sendRspToOutside(now, req.Request, page)
		// fmt.Printf(" sendRspToOutside: %0.9f,%s,RspToOutside,%s,%d\n", float64(now), tlb.outsidePort.Name(), req.Request.TaskID, page.VAddr)
		if !ok {
			return false
		}
		tlb.OutsidePort.Retrieve(now)
	}

	tlb.visit(setID, wayID)

	tracing.TraceReqReceive(req.Request, tlb)
	tracing.AddTaskStep(tracing.MsgIDAtReceiver(req.Request, tlb), tlb, "hit")
	tracing.TraceReqComplete(req.Request, tlb)
	tracing.StartTask(req.Request.TaskID,
		tracing.MsgIDAtReceiver(req.Request, tlb),
		tlb, "EvictTest", "*vm.TranslationReq", req.Request)

	return true
}

func (tlb *GMMUTLB) handleTranslationMiss(
	now sim.VTimeInSec,
	// req *vm.TranslationReq,
	mshrReq *MshrRequest,
) bool {
	if tlb.mshr.IsFull() {
		return false
	}

	fetched := tlb.fetchBottom(now, mshrReq)
	if fetched {
		if mshrReq.LocalFlag {
			tlb.topPort.Retrieve(now)
		} else if mshrReq.RemoteFlag {
			tlb.OutsidePort.Retrieve(now)
		}

		tracing.TraceReqReceive(mshrReq.Request, tlb)
		tracing.AddTaskStep(tracing.MsgIDAtReceiver(mshrReq.Request, tlb), tlb, "miss")
		tracing.StartTask(mshrReq.Request.TaskID,
			tracing.MsgIDAtReceiver(mshrReq.Request, tlb),
			tlb, "EvictTest", "*vm.TranslationReq", mshrReq.Request)
		return true
	}

	return false
}

func (tlb *GMMUTLB) vAddrToSetID(vAddr uint64) (setID int) {
	return int(vAddr / tlb.pageSize % uint64(tlb.numSets))
}

func (tlb *GMMUTLB) sendRspToTop(
	now sim.VTimeInSec,
	req *vm.TranslationReq,
	page vm.Page,
) bool {
	rsp := vm.TranslationRspBuilder{}.
		WithSendTime(now).
		WithSrc(tlb.topPort).
		WithDst(req.Src).
		WithRspTo(req.ID).
		WithPage(page).
		WithTaskID(req.TaskID).
		WithOriginPort(req.OriginPort).
		Build()

	err := tlb.topPort.Send(rsp)
	if err != nil {
		return false
	}

	// fmt.Printf("%0.9f,%s,RspToTop,%s\n",
	// 	float64(now), tlb.topPort.Name(), rsp.TaskID)

	return true
}

func (tlb *GMMUTLB) sendRspToOutside(
	now sim.VTimeInSec,
	req *vm.TranslationReq,
	page vm.Page,
) bool {
	rsp := vm.TranslationRspBuilder{}.
		WithSendTime(now).
		WithSrc(tlb.OutsidePort).
		WithDst(req.Src).
		WithRspTo(req.ID).
		WithPage(page).
		WithOriginPort(req.OriginPort).
		Build()

	err := tlb.OutsidePort.Send(rsp)
	if err != nil {
		return false
	}

	// fmt.Printf("%0.9f,%s,RspToTop,%s\n",
	// 	float64(now), tlb.topPort.Name(), rsp.TaskID)

	return true
}

func (tlb *GMMUTLB) processTLBMSHRHit(
	now sim.VTimeInSec,
	mshrEntry *mshrEntry,
	mshrReq *MshrRequest,
) bool {
	mshrEntry.Requests = append(mshrEntry.Requests, mshrReq)

	if mshrReq.LocalFlag {
		tlb.topPort.Retrieve(now)
		// fmt.Printf("%0.9f,%s,FetchReqFromTop,%s\n", float64(now), tlb.topPort.Name(), mshrReq.Request.TaskID)
	} else if mshrReq.RemoteFlag {
		tlb.OutsidePort.Retrieve(now)
		// fmt.Printf("%0.9f,%s,FetchReqFromOutside,%s\n", float64(now), tlb.OutsidePort.Name(), mshrReq.Request.TaskID)
	}

	tracing.TraceReqReceive(mshrReq.Request, tlb)
	tracing.AddTaskStep(tracing.MsgIDAtReceiver(mshrReq.Request, tlb), tlb, "mshr-hit")
	tracing.StartTask(mshrReq.Request.TaskID,
		tracing.MsgIDAtReceiver(mshrReq.Request, tlb),
		tlb, "EvictTest", "*vm.TranslationReq", mshrReq.Request)

	return true
}

func (tlb *GMMUTLB) fetchBottom(now sim.VTimeInSec, mshrReq *MshrRequest /*req *vm.TranslationReq*/) bool {
	page, found := tlb.pageTable.Find(mshrReq.Request.PID, mshrReq.Request.VAddr)
	if !found {
		panic("page not found")
	}

	if page.DeviceID != tlb.DeviceID {
		dstPort := tlb.PageFider.Find(page.DeviceID)

		toGMMUDistance := calculateDistance(int(tlb.DeviceID), int(page.DeviceID))
		toIOMMUDistance := calculateDistance(int(tlb.DeviceID), 0)

		if toGMMUDistance <= toIOMMUDistance {
			dstPort = dstPort
		} else {
			dstPort = tlb.IOMMUPort
		}

		fetchOutside := vm.TranslationReqBuilder{}.
			WithSendTime(now).
			WithSrc(tlb.OutsidePort).
			WithDst(dstPort).
			WithPID(mshrReq.Request.PID).
			WithVAddr(mshrReq.Request.VAddr).
			WithDeviceID(mshrReq.Request.DeviceID).
			WithTaskID(mshrReq.Request.TaskID).
			WithOriginPort(tlb.OutsidePort).
			Build()

		err := tlb.OutsidePort.Send(fetchOutside)
		if err != nil {
			return false
		}

		mshrEntry := tlb.mshr.Add(mshrReq.Request.PID, mshrReq.Request.VAddr)
		mshrEntry.Requests = append(mshrEntry.Requests, mshrReq)
		mshrEntry.reqToBottom = fetchOutside

		tracing.TraceReqInitiate(fetchOutside, tlb,
			tracing.MsgIDAtReceiver(mshrReq.Request, tlb))

		return true
	}

	fetchBottom := vm.TranslationReqBuilder{}.
		WithSendTime(now).
		WithSrc(tlb.bottomPort).
		WithDst(tlb.LowModule).
		WithPID(mshrReq.Request.PID).
		WithVAddr(mshrReq.Request.VAddr).
		WithDeviceID(mshrReq.Request.DeviceID).
		WithTaskID(mshrReq.Request.TaskID).
		WithOriginPort(tlb.bottomPort).
		Build()
	err := tlb.bottomPort.Send(fetchBottom)
	if err != nil {
		return false
	}

	mshrEntry := tlb.mshr.Add(mshrReq.Request.PID, mshrReq.Request.VAddr)
	mshrEntry.Requests = append(mshrEntry.Requests, mshrReq)
	mshrEntry.reqToBottom = fetchBottom

	tracing.TraceReqInitiate(fetchBottom, tlb,
		tracing.MsgIDAtReceiver(mshrReq.Request, tlb))

	// fmt.Printf("%0.9f,%s,FetchReqToBottom, %s, %d\n", float64(now), tlb.bottomPort.Name(), mshrReq.Request.TaskID, mshrReq.Request.VAddr)

	return true
}

func (tlb *GMMUTLB) parseBottom(now sim.VTimeInSec) bool {
	// if tlb.respondingMSHREntry != nil {
	// 	return false
	// }

	if len(tlb.respondingMSHREntry) != 0 {
		return false
	}

	item := tlb.bottomPort.Peek()
	if item == nil {
		return false
	}

	switch item := item.(type) {
	case *vm.TranslationRsp:
		return tlb.processRsp(now, item, true)
	default:
		panic("unexpected message type")
	}
}

func (tlb *GMMUTLB) performCtrlReq(now sim.VTimeInSec) bool {
	item := tlb.controlPort.Peek()
	if item == nil {
		return false
	}

	item = tlb.controlPort.Retrieve(now)

	switch req := item.(type) {
	case *FlushReq:
		return tlb.handleTLBFlush(now, req)
	case *RestartReq:
		return tlb.handleTLBRestart(now, req)
	default:
		log.Panicf("cannot process request %s", reflect.TypeOf(req))
	}

	return true
}

func (tlb *GMMUTLB) visit(setID, wayID int) {
	set := tlb.Sets[setID]
	set.Visit(wayID)
}

func (tlb *GMMUTLB) handleTLBFlush(now sim.VTimeInSec, req *FlushReq) bool {
	rsp := FlushRspBuilder{}.
		WithSrc(tlb.controlPort).
		WithDst(req.Src).
		WithSendTime(now).
		Build()

	err := tlb.controlPort.Send(rsp)
	if err != nil {
		return false
	}

	for _, vAddr := range req.VAddr {
		setID := tlb.vAddrToSetID(vAddr)
		set := tlb.Sets[setID]
		wayID, page, found := set.Lookup(req.PID, vAddr)
		if !found {
			continue
		}

		page.Valid = false
		set.Update(wayID, page)
	}

	tlb.mshr.Reset()
	tlb.isPaused = true
	return true
}

func (tlb *GMMUTLB) handleTLBRestart(now sim.VTimeInSec, req *RestartReq) bool {
	rsp := RestartRspBuilder{}.
		WithSendTime(now).
		WithSrc(tlb.controlPort).
		WithDst(req.Src).
		Build()

	err := tlb.controlPort.Send(rsp)
	if err != nil {
		return false
	}

	tlb.isPaused = false

	for tlb.topPort.Retrieve(now) != nil {
		tlb.topPort.Retrieve(now)
	}

	for tlb.bottomPort.Retrieve(now) != nil {
		tlb.bottomPort.Retrieve(now)
	}

	return true
}

func (tlb *GMMUTLB) processTranslation(now sim.VTimeInSec, req *vm.TranslationReq,
	localFlag, remoteFlag bool) bool {

	mshrReq := &MshrRequest{
		Request:    req,
		LocalFlag:  localFlag,
		RemoteFlag: remoteFlag,
	}

	mshrEntry := tlb.mshr.Query(req.PID, req.VAddr)
	if mshrEntry != nil {
		return tlb.processTLBMSHRHit(now, mshrEntry, mshrReq)
	}

	setID := tlb.vAddrToSetID(req.VAddr)
	set := tlb.Sets[setID]
	wayID, page, found := set.Lookup(req.PID, req.VAddr)
	if found && page.Valid {
		return tlb.handleTranslationHit(now, mshrReq, setID, wayID, page)
	}

	return tlb.handleTranslationMiss(now, mshrReq)
}

func (tlb *GMMUTLB) processRsp(now sim.VTimeInSec, rsp *vm.TranslationRsp, bottom bool) bool {
	// rsp := item.(*vm.TranslationRsp)
	page := rsp.Page

	mshrEntryPresent := tlb.mshr.IsEntryPresent(rsp.Page.PID, rsp.Page.VAddr)
	if !mshrEntryPresent {
		if bottom {
			tlb.bottomPort.Retrieve(now)
			// fmt.Printf("%0.9f,%s,RspFromBottom1,%s, %d, %d,%d\n", float64(now), tlb.bottomPort.Name(), rsp.TaskID, page.VAddr, tlb.deviceID, page.DeviceID)
		} else {
			tlb.OutsidePort.Retrieve(now)
			// fmt.Printf("%0.9f,%s,RspFromOutside1,%s, %d, %d,%d\n", float64(now), tlb.OutsidePort.Name(), rsp.TaskID, page.VAddr, tlb.deviceID, page.DeviceID)
		}
		return true
	}

	setID := tlb.vAddrToSetID(page.VAddr)
	set := tlb.Sets[setID]
	wayID, ok, oldPage := tlb.Sets[setID].Evict()
	if oldPage.VAddr != 0 {
		fmt.Printf("GPU[%d],%d,%d\n", tlb.DeviceID, page.VAddr, oldPage.VAddr)
	}

	if !ok {
		panic("failed to evict")
	}

	// if oldPage.Valid {
	// 	oldRsp := vm.TranslationRspBuilder{}.
	// 		WithSendTime(now).
	// 		WithSrc(tlb.controlPort).
	// 		WithDst(nil).
	// 		WithRspTo(tlb.topPort.Name()).
	// 		WithPage(oldPage).
	// 		WithTaskID(rsp.ID).
	// 		WithOriginPort(nil).
	// 		Build()
	// 	tracing.EndTask(oldRsp.ID, rsp)
	// }

	set.Update(wayID, page)
	set.Visit(wayID)

	mshrEntry := tlb.mshr.GetEntry(rsp.Page.PID, rsp.Page.VAddr)
	// tlb.respondingMSHREntry = mshrEntry
	tlb.respondingMSHREntry = append(tlb.respondingMSHREntry, mshrEntry)
	mshrEntry.page = page

	tlb.mshr.Remove(rsp.Page.PID, rsp.Page.VAddr)
	if bottom {
		tlb.bottomPort.Retrieve(now)
		// fmt.Printf("%0.9f,%s,RspFromBottom2,%s,%d, %d, %d\n", float64(now), tlb.bottomPort.Name(), rsp.TaskID, page.VAddr, tlb.deviceID, page.DeviceID)
	} else {
		tlb.OutsidePort.Retrieve(now)
		// if now == 0.000006025 {
		// 	print("rspToTop: ")
		// }
		// fmt.Printf("%0.9f,%s,RspFromOutside2,%s,%d, %d, %d\n", float64(now), tlb.OutsidePort.Name(), rsp.TaskID, page.VAddr, tlb.deviceID, page.DeviceID)
	}
	tracing.TraceReqFinalize(mshrEntry.reqToBottom, tlb)
	return true
}

func getCoordinates(id int) (int, int) {
	if id == 0 {
		return 3, 3 // 特殊处理 0 的坐标
	}
	row := (id - 1) / 7
	col := (id - 1) % 7
	return row, col
}

func calculateDistance(id1, id2 int) float64 {
	x1, y1 := getCoordinates(id1)
	x2, y2 := getCoordinates(id2)

	// 计算曼哈顿距离
	distance := math.Abs(float64(x1-x2)) + math.Abs(float64(y1-y2))

	return distance
}
