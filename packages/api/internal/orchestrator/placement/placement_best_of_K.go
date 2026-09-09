package placement

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"

	"github.com/e2b-dev/infra/packages/api/internal/orchestrator/nodemanager"
)

// BestOfKConfig holds the configuration parameters for the placement algorithm
type BestOfKConfig struct {
	// R is the cluster-wide max over-commit ratio
	R float64
	// Alpha is the weight for CPU usage in the score calculation
	Alpha float64
	// K is the number of candidate nodes sampled per placement ("power of K choices")
	K int
	// HybridPlacement enables deterministic occupied-before-empty placement over all eligible nodes.
	HybridPlacement bool
	// HybridThreshold is the count below which placement binpacks onto the most-loaded occupied node.
	HybridThreshold int
	// HybridCeiling is the hard running-plus-starting count accepted by placement.
	HybridCeiling int
}

// DefaultBestOfKConfig returns the default placement configuration
func DefaultBestOfKConfig() BestOfKConfig {
	return BestOfKConfig{
		R:               4,
		K:               3,
		Alpha:           0.5,
		HybridThreshold: 4,
		HybridCeiling:   4,
	}
}

// Score calculates the placement score for this node
func (b *BestOfK) Score(node *nodemanager.Node, resources nodemanager.SandboxResources, config BestOfKConfig) float64 {
	metrics := node.Metrics()

	// Get locally recorded resources that haven't been reported yet.
	pendingCPUs := int64(0)
	for _, res := range node.PlacementMetrics.InProgress() {
		pendingCPUs += res.CPUs
	}

	// Combine allocated resources with in-progress allocations
	reserved := metrics.CpuAllocated + uint32(pendingCPUs)

	// 1 CPU used = 100% CPU percept
	usageAvg := float64(metrics.CpuPercent) / 100

	// to avoid division by zero
	cpuCount := float64(metrics.CpuCount)
	if cpuCount == 0 {
		return math.MaxFloat64
	}

	totalCapacity := config.R * cpuCount

	cpuRequested := float64(resources.CPUs)

	return (cpuRequested + float64(reserved) + config.Alpha*usageAvg) / totalCapacity
}

// BestOfK implements the fit-score-place algorithm
type BestOfK struct {
	config BestOfKConfig
	mu     sync.RWMutex
}

var _ Algorithm = &BestOfK{}

// NewBestOfK creates a new placement algorithm with the given config
func NewBestOfK(config BestOfKConfig) Algorithm {
	return &BestOfK{
		config: config,
	}
}

func (b *BestOfK) getConfig() BestOfKConfig {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return b.config
}

// UpdateConfig updates the BestOfK algorithm configuration
func (b *BestOfK) UpdateConfig(config BestOfKConfig) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.config = config
}

// chooseNode selects the best node for placing a VM with the given quota
func (b *BestOfK) chooseNode(_ context.Context, nodes []*nodemanager.Node, excludedNodes map[string]struct{}, resources nodemanager.SandboxResources, cpu CPURequirement, features FeatureRequirement, filterByLabels bool, requiredLabels []string) (bestNode *nodemanager.Node, err error) {
	// Fix the config, we want to dynamically update it
	config := b.getConfig()

	if config.HybridPlacement {
		candidates := b.eligible(nodes, config, excludedNodes, cpu, features, filterByLabels, requiredLabels)

		return b.chooseHybrid(candidates, config, cpu, features, filterByLabels, requiredLabels)
	}

	// Filter eligible nodes
	candidates := b.sample(nodes, config, excludedNodes, cpu, features, filterByLabels, requiredLabels)

	// Find the best node among candidates
	bestScore := math.MaxFloat64

	for _, node := range candidates {
		// Calculate score
		score := b.Score(node, resources, config)

		if score < bestScore {
			bestNode = node
			bestScore = score
		}
	}

	if bestNode == nil {
		return nil, FailedToPlaceSandboxError{
			filterByLabels: filterByLabels,
			requiredLabels: requiredLabels,
			cpu:            cpu,
			features:       features,
		}
	}

	return bestNode, nil
}
func (b *BestOfK) tryReserve(node *nodemanager.Node, sandboxID string, resources nodemanager.SandboxResources) bool {
	config := b.getConfig()
	if !config.HybridPlacement {
		node.PlacementMetrics.StartPlacing(sandboxID, resources)

		return true
	}

	if config.HybridCeiling <= 0 {
		return false
	}

	return node.PlacementMetrics.TryStartPlacing(sandboxID, resources, b.runningOrStartingCount(node), uint32(config.HybridCeiling))
}

func (b *BestOfK) runningOrStartingCount(node *nodemanager.Node) uint32 {
	metrics := node.Metrics()

	return metrics.SandboxCount + metrics.SandboxStartingCount
}

func (b *BestOfK) chooseHybrid(candidates []*nodemanager.Node, config BestOfKConfig, cpu CPURequirement, features FeatureRequirement, filterByLabels bool, requiredLabels []string) (*nodemanager.Node, error) {
	threshold := config.HybridThreshold
	if threshold <= 0 {
		threshold = 4
	}

	type candidate struct {
		node  *nodemanager.Node
		count uint32
	}

	var belowThreshold *candidate
	var occupied *candidate
	var empty *candidate
	for _, node := range candidates {
		count := b.runningOrStartingCount(node) + node.PlacementMetrics.InProgressCount()
		current := &candidate{node: node, count: count}
		switch {
		case count == 0:
			if empty == nil || strings.Compare(node.ID, empty.node.ID) < 0 {
				empty = current
			}
		case int(count) < threshold:
			if belowThreshold == nil || count > belowThreshold.count ||
				(count == belowThreshold.count && strings.Compare(node.ID, belowThreshold.node.ID) < 0) {
				belowThreshold = current
			}
		default:
			if occupied == nil || count < occupied.count ||
				(count == occupied.count && strings.Compare(node.ID, occupied.node.ID) < 0) {
				occupied = current
			}
		}
	}

	if belowThreshold != nil {
		return belowThreshold.node, nil
	}
	if occupied != nil {
		return occupied.node, nil
	}
	if empty != nil {
		return empty.node, nil
	}

	return nil, FailedToPlaceSandboxError{
		filterByLabels: filterByLabels,
		requiredLabels: requiredLabels,
		cpu:            cpu,
		features:       features,
	}
}

func (b *BestOfK) eligible(items []*nodemanager.Node, config BestOfKConfig, excludedNodes map[string]struct{}, cpu CPURequirement, features FeatureRequirement, filterByLabels bool, requiredLabels []string) []*nodemanager.Node {
	if config.HybridCeiling <= 0 {
		return nil
	}

	candidates := make([]*nodemanager.Node, 0, len(items))
	for _, node := range items {
		if _, ok := excludedNodes[node.ID]; ok {
			continue
		}
		if !node.CanAcceptNewRequests() {
			continue
		}
		if !NodeSatisfiesCPU(node, cpu) {
			continue
		}
		if !NodeSatisfiesFeatures(node, features) {
			continue
		}
		if filterByLabels && !isNodeLabelsCompatible(node, requiredLabels) {
			continue
		}
		if b.runningOrStartingCount(node)+node.PlacementMetrics.InProgressCount() >= uint32(config.HybridCeiling) {
			continue
		}

		candidates = append(candidates, node)
	}

	return candidates
}

type FailedToPlaceSandboxError struct {
	filterByLabels bool
	requiredLabels []string
	cpu            CPURequirement
	features       FeatureRequirement
}

var _ error = FailedToPlaceSandboxError{}

func (e FailedToPlaceSandboxError) Error() string {
	message := fmt.Sprintf("no node available with required metadata: machine=%v", e.cpu.Build)

	if e.cpu.PinnedModel != "" {
		message += fmt.Sprintf(", cpu_model_pinned=%s", e.cpu.PinnedModel)
	}

	if e.filterByLabels {
		message += fmt.Sprintf(", labels=%v", e.requiredLabels)
	}

	if e.features.MinVersion() != "" {
		message += fmt.Sprintf(", features=%v, min_orchestrator_version=%s", e.features.FeatureNames(), e.features.MinVersion())
	}

	return message
}

// sample returns up to k items chosen uniformly from those passing ok.
func (b *BestOfK) sample(items []*nodemanager.Node, config BestOfKConfig, excludedNodes map[string]struct{}, cpu CPURequirement, features FeatureRequirement, filterByLabels bool, requiredLabels []string) []*nodemanager.Node {
	if config.K <= 0 || len(items) == 0 {
		return nil
	}

	indices := make([]int, len(items))
	for i := range indices {
		indices[i] = i
	}

	candidates := make([]*nodemanager.Node, 0, config.K)
	remaining := len(indices) // active pool is indices[:remaining]

	for len(candidates) < config.K && remaining > 0 {
		// pick from the active pool
		j := rand.Intn(remaining)
		pick := indices[j]

		// remove j from pool
		indices[j], indices[remaining-1] = indices[remaining-1], indices[j]
		remaining--

		n := items[pick]

		// Excluded filter
		if _, ok := excludedNodes[n.ID]; ok {
			continue
		}

		// If the node can't take new sandboxes, skip it
		if !n.CanAcceptNewRequests() {
			continue
		}

		// Skip if node is not CPU compatible
		if !NodeSatisfiesCPU(n, cpu) {
			continue
		}

		// Skip if the node's orchestrator predates a requested feature
		if !NodeSatisfiesFeatures(n, features) {
			continue
		}

		// Skip if node doesn't have the required labels
		if filterByLabels && !isNodeLabelsCompatible(n, requiredLabels) {
			continue
		}

		candidates = append(candidates, n)
	}

	return candidates
}
