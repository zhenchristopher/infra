package placement

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/api/internal/orchestrator/nodemanager"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
	"github.com/e2b-dev/infra/packages/shared/pkg/machineinfo"
)

func hybridTestNode(id string, status api.NodeStatus, running, starting uint32) *nodemanager.Node {
	return nodemanager.NewTestNode(id, status, 0, 16, nodemanager.WithSandboxCounts(running, starting))
}

func hybridTestAlgorithm() *BestOfK {
	return NewBestOfK(BestOfKConfig{
		R:               4,
		K:               1,
		Alpha:           0.5,
		CanFit:          true,
		HybridPlacement: true,
		HybridThreshold: 4,
		HybridCeiling:   4,
	}).(*BestOfK)
}

func chooseHybridTestNode(t *testing.T, nodes ...*nodemanager.Node) *nodemanager.Node {
	t.Helper()

	selected, err := hybridTestAlgorithm().chooseNode(
		t.Context(),
		nodes,
		map[string]struct{}{},
		nodemanager.SandboxResources{CPUs: 1, MiBMemory: 512},
		machineinfo.MachineInfo{},
		false,
		nil,
	)
	require.NoError(t, err)

	return selected
}

func TestHybridPlacement_BinpacksOccupiedNodesBelowThreshold(t *testing.T) {
	t.Parallel()

	selected := chooseHybridTestNode(t,
		hybridTestNode("one", api.NodeStatusReady, 1, 0),
		hybridTestNode("three", api.NodeStatusReady, 3, 0),
		hybridTestNode("empty", api.NodeStatusReady, 0, 0),
	)

	assert.Equal(t, "three", selected.ID)
}

func TestHybridPlacement_UsesEmptyAfterOccupiedNodesReachCeiling(t *testing.T) {
	t.Parallel()

	selected := chooseHybridTestNode(t,
		hybridTestNode("four", api.NodeStatusReady, 4, 0),
		hybridTestNode("empty", api.NodeStatusReady, 0, 0),
	)

	assert.Equal(t, "empty", selected.ID)
}

func TestHybridPlacement_UsesEmptyOnlyWhenNoOccupiedNodeFits(t *testing.T) {
	t.Parallel()

	selected := chooseHybridTestNode(t,
		hybridTestNode("full-a", api.NodeStatusReady, 4, 0),
		hybridTestNode("full-b", api.NodeStatusReady, 3, 1),
		hybridTestNode("empty", api.NodeStatusReady, 0, 0),
	)

	assert.Equal(t, "empty", selected.ID)
}

func TestHybridPlacement_InspectsFullEligibleSetAndCountsStarting(t *testing.T) {
	t.Parallel()

	selected := chooseHybridTestNode(t,
		hybridTestNode("empty-a", api.NodeStatusReady, 0, 0),
		hybridTestNode("empty-b", api.NodeStatusReady, 0, 0),
		hybridTestNode("occupied-outside-k", api.NodeStatusReady, 2, 1),
	)

	assert.Equal(t, "occupied-outside-k", selected.ID)
}

func TestHybridPlacement_ExcludesDrainingNodes(t *testing.T) {
	t.Parallel()

	selected := chooseHybridTestNode(t,
		hybridTestNode("draining", api.NodeStatusDraining, 3, 0),
		hybridTestNode("ready", api.NodeStatusReady, 1, 0),
	)

	assert.Equal(t, "ready", selected.ID)
}

func TestHybridPlacement_ExcludesNodesThatCannotFit(t *testing.T) {
	t.Parallel()

	algorithm := hybridTestAlgorithm()
	selected, err := algorithm.chooseNode(
		t.Context(),
		[]*nodemanager.Node{
			nodemanager.NewTestNode("occupied-without-capacity", api.NodeStatusReady, 64, 16, nodemanager.WithSandboxCounts(3, 0)),
			hybridTestNode("empty-with-capacity", api.NodeStatusReady, 0, 0),
		},
		map[string]struct{}{},
		nodemanager.SandboxResources{CPUs: 1, MiBMemory: 512},
		machineinfo.MachineInfo{},
		false,
		nil,
	)
	require.NoError(t, err)
	assert.Equal(t, "empty-with-capacity", selected.ID)
}

func TestHybridPlacement_PreservesCompatibilityAndLabelFilters(t *testing.T) {
	t.Parallel()

	buildMachine := machineinfo.MachineInfo{CPUArchitecture: "x86_64", CPUFamily: "Intel", CPUModel: "85"}
	algorithm := hybridTestAlgorithm()
	selected, err := algorithm.chooseNode(
		t.Context(),
		[]*nodemanager.Node{
			nodemanager.NewTestNode("wrong-cpu", api.NodeStatusReady, 0, 16,
				nodemanager.WithSandboxCounts(3, 0),
				nodemanager.WithCPUInfo("aarch64", "ARM", "8"),
				nodemanager.WithLabels([]string{"scaffold"}),
			),
			nodemanager.NewTestNode("wrong-label", api.NodeStatusReady, 0, 16,
				nodemanager.WithSandboxCounts(2, 0),
				nodemanager.WithCPUInfo("x86_64", "Intel", "85"),
				nodemanager.WithLabels([]string{"other"}),
			),
			nodemanager.NewTestNode("compatible-empty", api.NodeStatusReady, 0, 16,
				nodemanager.WithCPUInfo("x86_64", "Intel", "85"),
				nodemanager.WithLabels([]string{"scaffold"}),
			),
		},
		map[string]struct{}{},
		nodemanager.SandboxResources{CPUs: 1, MiBMemory: 512},
		buildMachine,
		true,
		[]string{"scaffold"},
	)
	require.NoError(t, err)
	assert.Equal(t, "compatible-empty", selected.ID)
}

func TestHybridPlacement_TieBreaksByNodeID(t *testing.T) {
	t.Parallel()

	selected := chooseHybridTestNode(t,
		hybridTestNode("z-node", api.NodeStatusReady, 3, 0),
		hybridTestNode("a-node", api.NodeStatusReady, 3, 0),
	)

	assert.Equal(t, "a-node", selected.ID)
}

func TestHybridPlacement_AtomicReservationPreventsFifthLocalPlacement(t *testing.T) {
	t.Parallel()

	node := hybridTestNode("node", api.NodeStatusReady, 3, 0)
	resources := nodemanager.SandboxResources{CPUs: 1, MiBMemory: 512}

	var wg sync.WaitGroup
	results := make(chan bool, 2)
	for _, sandboxID := range []string{"first", "second"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- node.PlacementMetrics.TryStartPlacing(sandboxID, resources, node.Metrics().SandboxCount, 4)
		}()
	}
	wg.Wait()
	close(results)

	successes := 0
	for result := range results {
		if result {
			successes++
		}
	}
	assert.Equal(t, 1, successes)
}

func TestHybridPlacement_PreferredNodeAtCeilingFallsBack(t *testing.T) {
	t.Parallel()

	preferred := hybridTestNode("preferred-full", api.NodeStatusReady, 4, 0)
	fallback := hybridTestNode("fallback", api.NodeStatusReady, 1, 0)
	nodes := []*nodemanager.Node{preferred, fallback}
	request := &orchestrator.SandboxCreateRequest{
		Sandbox: &orchestrator.SandboxConfig{
			SandboxId: "sandbox",
			Vcpu:      1,
			RamMb:     512,
		},
	}

	selected, err := PlaceSandbox(t.Context(), hybridTestAlgorithm(), nodes, preferred, request, machineinfo.MachineInfo{}, false, nil)
	require.NoError(t, err)
	assert.Equal(t, fallback, selected)
}

func TestHybridPlacement_InvalidCeilingFailsClosed(t *testing.T) {
	t.Parallel()

	config := DefaultBestOfKConfig()
	config.CanFit = true
	config.HybridPlacement = true
	config.HybridCeiling = -1
	algorithm := NewBestOfK(config).(*BestOfK)
	node := hybridTestNode("node", api.NodeStatusReady, 1, 0)
	resources := nodemanager.SandboxResources{CPUs: 1, MiBMemory: 512}

	selected, err := algorithm.chooseNode(
		t.Context(),
		[]*nodemanager.Node{node},
		map[string]struct{}{},
		resources,
		machineinfo.MachineInfo{},
		false,
		nil,
	)
	require.Error(t, err)
	assert.Nil(t, selected)
	assert.False(t, algorithm.tryReserve(node, "sandbox", resources))
}

func TestHybridPlacement_DisabledPreservesLegacyBestOfKPath(t *testing.T) {
	t.Parallel()

	config := DefaultBestOfKConfig()
	config.K = 1
	node := hybridTestNode("legacy-node", api.NodeStatusReady, 8, 0)

	selected, err := NewBestOfK(config).(*BestOfK).chooseNode(
		t.Context(),
		[]*nodemanager.Node{node},
		map[string]struct{}{},
		nodemanager.SandboxResources{CPUs: 1, MiBMemory: 512},
		machineinfo.MachineInfo{},
		false,
		nil,
	)
	require.NoError(t, err)
	assert.Equal(t, node, selected)
}
