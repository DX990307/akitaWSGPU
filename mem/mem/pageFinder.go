package mem

import "github.com/sarchlab/akita/v3/sim"

type PageFinder interface {
	Find(deviceID uint64) sim.Port
}

type MultiPageFinder struct {
	LowModules map[uint64]sim.Port
}

func (f *MultiPageFinder) Find(deviceID uint64) sim.Port {
	return f.LowModules[deviceID]
}

func NewMultiPageFinder() *MultiPageFinder {
	finder := new(MultiPageFinder)
	finder.LowModules = make(map[uint64]sim.Port)
	return finder
}
