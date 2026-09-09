package nodemanager

import (
	"sync"
	"sync/atomic"

	"github.com/e2b-dev/infra/packages/shared/pkg/smap"
)

type SandboxResources struct {
	CPUs      int64
	MiBMemory int64
}

type PlacementMetrics struct {
	sandboxesInProgress *smap.Map[SandboxResources]
	reservationMu       sync.Mutex

	createSuccess atomic.Uint64
	createFails   atomic.Uint64
}

func (p *PlacementMetrics) Success(sandboxID string) {
	p.createSuccess.Add(1)
	p.removeSandbox(sandboxID)
}

func (p *PlacementMetrics) Skip(sandboxID string) {
	p.removeSandbox(sandboxID)
}

func (p *PlacementMetrics) Fail(sandboxID string) {
	p.createFails.Add(1)
	p.removeSandbox(sandboxID)
}

func (p *PlacementMetrics) SuccessCount() uint64 {
	return p.createSuccess.Load()
}

func (p *PlacementMetrics) FailsCount() uint64 {
	return p.createFails.Load()
}

func (p *PlacementMetrics) InProgress() map[string]SandboxResources {
	return p.sandboxesInProgress.Items()
}

func (p *PlacementMetrics) InProgressCount() uint32 {
	return uint32(p.sandboxesInProgress.Count())
}

func (p *PlacementMetrics) StartPlacing(sandboxID string, resources SandboxResources) {
	p.sandboxesInProgress.Insert(sandboxID, resources)
}

func (p *PlacementMetrics) TryStartPlacing(sandboxID string, resources SandboxResources, observedHostCount uint32, ceiling uint32) bool {
	p.reservationMu.Lock()
	defer p.reservationMu.Unlock()

	if ceiling == 0 || observedHostCount+p.InProgressCount() >= ceiling {
		return false
	}

	p.sandboxesInProgress.Insert(sandboxID, resources)

	return true
}

func (p *PlacementMetrics) removeSandbox(sandboxID string) {
	p.sandboxesInProgress.Remove(sandboxID)
}
