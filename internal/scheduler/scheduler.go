package scheduler

import (
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/hivehq/hive/internal/config"
	"github.com/hivehq/hive/internal/state"
	"github.com/hivehq/hive/internal/types"
)

// Assignment represents a scheduling decision mapping an agent to a node.
type Assignment struct {
	AgentID string
	NodeID  string
}

// nodeAllocation tracks resources allocated on a node.
type nodeAllocation struct {
	MemoryUsed int64
	VCPUsUsed  int
}

// Scheduler assigns agents to Tier 1 nodes based on resource availability,
// placement constraints, and scoring heuristics.
type Scheduler struct {
	mu          sync.Mutex
	store       *state.Store
	logger      *slog.Logger
	allocations map[string]*nodeAllocation // nodeID -> allocated resources
}

// NewScheduler creates a new Scheduler backed by the given state store.
func NewScheduler(store *state.Store, logger *slog.Logger) *Scheduler {
	s := &Scheduler{
		store:       store,
		logger:      logger,
		allocations: make(map[string]*nodeAllocation),
	}
	s.rebuildAllocations()
	return s
}

// rebuildAllocations reconstructs in-memory allocation tracking from the
// current state store. This is called on startup and can be called to
// resynchronize after external state changes.
func (s *Scheduler) rebuildAllocations() {
	s.allocations = make(map[string]*nodeAllocation)

	nodes := s.store.AllNodes()
	for _, n := range nodes {
		s.allocations[n.ID] = &nodeAllocation{}
	}

	// Walk through agents and accumulate their resource usage on each node.
	agents := s.store.AllAgents()
	for _, a := range agents {
		if a.NodeID == "" {
			continue
		}
		alloc, ok := s.allocations[a.NodeID]
		if !ok {
			alloc = &nodeAllocation{}
			s.allocations[a.NodeID] = alloc
		}
		alloc.MemoryUsed += a.MemoryBytes
		alloc.VCPUsUsed += a.VCPUs
	}
}

// Schedule assigns the given agent manifest to the best-fit node.
// Returns an Assignment on success or an error if no suitable node is available.
func (s *Scheduler) Schedule(manifest *types.AgentManifest) (*Assignment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	agentID := manifest.Metadata.ID

	// Parse agent resource requirements.
	memNeeded, err := agentMemoryBytes(manifest)
	if err != nil {
		return nil, fmt.Errorf("parsing agent %s memory: %w", agentID, err)
	}
	vcpusNeeded := manifest.Spec.Resources.VCPUs
	if vcpusNeeded == 0 {
		vcpusNeeded = 1 // default
	}

	// If placement.nodeId is set, use that node directly (override).
	if manifest.Spec.Placement.NodeID != "" {
		node := s.store.GetNode(manifest.Spec.Placement.NodeID)
		if node == nil {
			return nil, fmt.Errorf("placement node %q not found", manifest.Spec.Placement.NodeID)
		}
		if err := s.validateNode(node, manifest, memNeeded, vcpusNeeded); err != nil {
			return nil, fmt.Errorf("placement node %q not eligible: %w", node.ID, err)
		}
		s.allocate(node.ID, memNeeded, vcpusNeeded)
		s.logger.Info("scheduled agent via placement override",
			"agent_id", agentID,
			"node_id", node.ID,
		)
		return &Assignment{AgentID: agentID, NodeID: node.ID}, nil
	}

	// Filter eligible nodes.
	nodes := s.store.AllNodes()
	var candidates []*types.NodeState
	for _, n := range nodes {
		if err := s.validateNode(n, manifest, memNeeded, vcpusNeeded); err != nil {
			s.logger.Debug("node excluded",
				"node_id", n.ID,
				"agent_id", agentID,
				"reason", err.Error(),
			)
			continue
		}
		candidates = append(candidates, n)
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("no eligible node for agent %s: agent should be PENDING", agentID)
	}

	// Score candidates and pick the best.
	best := s.scoreAndSelect(candidates, manifest, memNeeded, vcpusNeeded)

	s.allocate(best.ID, memNeeded, vcpusNeeded)
	s.logger.Info("scheduled agent",
		"agent_id", agentID,
		"node_id", best.ID,
	)
	return &Assignment{AgentID: agentID, NodeID: best.ID}, nil
}

// ReleaseAgent frees the resources allocated to the given agent.
func (s *Scheduler) ReleaseAgent(agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	agent := s.store.GetAgent(agentID)
	if agent == nil {
		return fmt.Errorf("agent %q not found in state", agentID)
	}
	if agent.NodeID == "" {
		return nil // nothing to release
	}

	alloc, ok := s.allocations[agent.NodeID]
	if !ok {
		return nil
	}

	alloc.MemoryUsed -= agent.MemoryBytes
	if alloc.MemoryUsed < 0 {
		alloc.MemoryUsed = 0
	}
	alloc.VCPUsUsed -= agent.VCPUs
	if alloc.VCPUsUsed < 0 {
		alloc.VCPUsUsed = 0
	}

	s.logger.Info("released agent resources",
		"agent_id", agentID,
		"node_id", agent.NodeID,
	)
	return nil
}

// validateNode checks whether a node is eligible for the given agent.
func (s *Scheduler) validateNode(node *types.NodeState, manifest *types.AgentManifest, memNeeded int64, vcpusNeeded int) error {
	// Must be Tier 1 (KVM capable).
	if node.Tier != types.NodeTier1 {
		return fmt.Errorf("not tier 1")
	}

	// Must have KVM available.
	if !node.Resources.KVMAvail {
		return fmt.Errorf("KVM not available")
	}

	// Must not be cordoned or draining.
	if node.Status == types.NodeStatusCordoned {
		return fmt.Errorf("node is cordoned")
	}
	if node.Status == types.NodeStatusDraining {
		return fmt.Errorf("node is draining")
	}

	// Must be online.
	if node.Status != types.NodeStatusOnline {
		return fmt.Errorf("node status is %s, not online", node.Status)
	}

	// Architecture compatibility.
	if manifest.Spec.Placement.Arch != "" && node.Arch != manifest.Spec.Placement.Arch {
		return fmt.Errorf("arch mismatch: need %s, have %s", manifest.Spec.Placement.Arch, node.Arch)
	}

	// Label selectors from placement.nodeLabels.
	for key, val := range manifest.Spec.Placement.NodeLabels {
		nodeVal, ok := node.Labels[key]
		if !ok || nodeVal != val {
			return fmt.Errorf("label %s=%s not matched (node has %q)", key, val, nodeVal)
		}
	}

	// Sufficient resources (check against total minus already allocated).
	alloc := s.getAlloc(node.ID)
	availableMemory := node.Resources.MemoryTotal - alloc.MemoryUsed
	if memNeeded > availableMemory {
		return fmt.Errorf("insufficient memory: need %d, available %d", memNeeded, availableMemory)
	}
	availableVCPUs := node.Resources.CPUCount - alloc.VCPUsUsed
	if vcpusNeeded > availableVCPUs {
		return fmt.Errorf("insufficient vcpus: need %d, available %d", vcpusNeeded, availableVCPUs)
	}

	return nil
}

// scoreAndSelect scores all candidate nodes and returns the best one.
// Scoring:
//   - available_memory_after/total_memory (weight 1.0)
//   - available_vcpus_after/total_vcpus (weight 1.0)
//   - team_colocation_bonus (+0.5 if node has same-team agent)
//   - spread_penalty (-0.3 if most loaded node)
//
// Highest score wins. Tie: alphabetical node ID.
func (s *Scheduler) scoreAndSelect(candidates []*types.NodeState, manifest *types.AgentManifest, memNeeded int64, vcpusNeeded int) *types.NodeState {
	type scored struct {
		node  *types.NodeState
		score float64
	}

	// Determine which node is most loaded (for spread penalty).
	var maxLoadRatio float64
	var mostLoadedID string
	for _, n := range candidates {
		alloc := s.getAlloc(n.ID)
		loadRatio := 0.0
		if n.Resources.MemoryTotal > 0 {
			loadRatio += float64(alloc.MemoryUsed) / float64(n.Resources.MemoryTotal)
		}
		if n.Resources.CPUCount > 0 {
			loadRatio += float64(alloc.VCPUsUsed) / float64(n.Resources.CPUCount)
		}
		if loadRatio > maxLoadRatio || (loadRatio == maxLoadRatio && (mostLoadedID == "" || n.ID < mostLoadedID)) {
			maxLoadRatio = loadRatio
			mostLoadedID = n.ID
		}
	}

	// Build list of same-team agents to check colocation.
	teamID := manifest.Metadata.Team
	teamNodeSet := make(map[string]bool)
	if teamID != "" {
		agents := s.store.AllAgents()
		for _, a := range agents {
			if a.Team == teamID && a.NodeID != "" {
				teamNodeSet[a.NodeID] = true
			}
		}
	}

	var scores []scored
	for _, n := range candidates {
		alloc := s.getAlloc(n.ID)

		// Memory score: remaining memory after allocation / total.
		memAfter := float64(n.Resources.MemoryTotal-alloc.MemoryUsed-memNeeded) / float64(n.Resources.MemoryTotal)

		// VCPU score: remaining vcpus after allocation / total.
		vcpuAfter := 0.0
		if n.Resources.CPUCount > 0 {
			vcpuAfter = float64(n.Resources.CPUCount-alloc.VCPUsUsed-vcpusNeeded) / float64(n.Resources.CPUCount)
		}

		score := memAfter*1.0 + vcpuAfter*1.0

		// Team colocation bonus.
		if teamID != "" && teamNodeSet[n.ID] {
			score += 0.5
		}

		// Spread penalty: penalize the most loaded node.
		if n.ID == mostLoadedID && len(candidates) > 1 {
			score -= 0.3
		}

		scores = append(scores, scored{node: n, score: score})
	}

	// Sort: highest score first, then alphabetical node ID for tiebreaking.
	sort.Slice(scores, func(i, j int) bool {
		if scores[i].score != scores[j].score {
			return scores[i].score > scores[j].score
		}
		return scores[i].node.ID < scores[j].node.ID
	})

	return scores[0].node
}

// getAlloc returns the current allocation for a node, creating one if needed.
func (s *Scheduler) getAlloc(nodeID string) *nodeAllocation {
	alloc, ok := s.allocations[nodeID]
	if !ok {
		alloc = &nodeAllocation{}
		s.allocations[nodeID] = alloc
	}
	return alloc
}

// allocate records a resource allocation on a node.
func (s *Scheduler) allocate(nodeID string, memBytes int64, vcpus int) {
	alloc := s.getAlloc(nodeID)
	alloc.MemoryUsed += memBytes
	alloc.VCPUsUsed += vcpus
}

// agentMemoryBytes parses the agent's memory requirement into bytes.
// Returns 0 if no memory is specified.
func agentMemoryBytes(manifest *types.AgentManifest) (int64, error) {
	if manifest.Spec.Resources.Memory == "" {
		return 0, nil
	}
	return config.ParseMemory(manifest.Spec.Resources.Memory)
}
