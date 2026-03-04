// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

// Lock ordering: scheduler.mu must be acquired before store.mu.
// This means any store method called while holding scheduler.mu must only
// acquire store.mu (not scheduler.mu). To avoid potential deadlocks, prefer
// releasing scheduler.mu before calling store methods that perform writes
// (e.g., SetAgent), unless the operation requires atomicity with the
// in-memory allocation map.

// Package scheduler implements bin-packing placement of agents onto nodes based on available CPU and memory.
package scheduler

import (
	"fmt"
	"log/slog"
	"math"
	"sort"
	"sync"

	"github.com/brmurrell3/hive/internal/config"
	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/types"
)

// Assignment represents a scheduling decision mapping an agent to a node.
type Assignment struct {
	AgentID     string
	NodeID      string
	BackendType string // backend selected for this agent (e.g., "firecracker", "process", "nspawn")
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
	dirty       bool                       // true when allocations need rebuild from store
}

// NewScheduler creates a new Scheduler backed by the given state store.
func NewScheduler(store *state.Store, logger *slog.Logger) *Scheduler {
	s := &Scheduler{
		store:       store,
		logger:      logger,
		allocations: make(map[string]*nodeAllocation),
		dirty:       true, // force initial rebuild
	}
	s.rebuildAllocations()
	return s
}

// rebuildAllocations reconstructs in-memory allocation tracking from the
// current state store. This is called on startup and when the dirty flag
// is set (e.g., after ReleaseAgent or SyncAllocations). Skips the rebuild
// if allocations are already up-to-date (dirty == false) to prevent
// discarding in-flight allocate() calls that haven't been persisted yet.
func (s *Scheduler) rebuildAllocations() {
	if !s.dirty {
		return
	}

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

	s.dirty = false
}

// SyncAllocations forces a rebuild of the in-memory allocation tracking
// from the current state store. This should be called when external state
// changes are known to have occurred (e.g., agents removed outside the
// scheduler) to resynchronize allocations.
func (s *Scheduler) SyncAllocations() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dirty = true
	s.rebuildAllocations()
}

// Schedule assigns the given agent manifest to the best-fit node.
// Returns an Assignment on success or an error if no suitable node is available.
func (s *Scheduler) Schedule(manifest *types.AgentManifest) (*Assignment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Sync allocations from the state store to prevent drift from external
	// modifications (e.g., agents removed outside the scheduler).
	s.rebuildAllocations()

	agentID := manifest.Metadata.ID
	if agentID == "" {
		return nil, fmt.Errorf("manifest has empty Metadata.ID")
	}

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
		return &Assignment{AgentID: agentID, NodeID: node.ID, BackendType: resolveBackendType(manifest)}, nil
	}

	// Filter eligible nodes.
	nodes := s.store.AllNodes()
	candidates := make([]*types.NodeState, 0, len(nodes))
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
	if best == nil {
		return nil, fmt.Errorf("no suitable node found for agent %s", agentID)
	}

	s.allocate(best.ID, memNeeded, vcpusNeeded)
	s.logger.Info("scheduled agent",
		"agent_id", agentID,
		"node_id", best.ID,
		"backend", resolveBackendType(manifest),
	)
	return &Assignment{AgentID: agentID, NodeID: best.ID, BackendType: resolveBackendType(manifest)}, nil
}

// ScheduleNodeID is a convenience method that wraps Schedule to return only
// the node ID.
func (s *Scheduler) ScheduleNodeID(manifest *types.AgentManifest) (string, error) {
	assignment, err := s.Schedule(manifest)
	if err != nil {
		return "", err
	}
	return assignment.NodeID, nil
}

// ReleaseAgent frees the resources allocated to the given agent.
// It uses store.ModifyAgent to atomically zero NodeID/MemoryBytes/VCPUs
// while still holding s.mu, ensuring the in-memory allocation map and
// the store are updated consistently. This is safe because the documented
// lock ordering is scheduler.mu -> store.mu.
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
		s.logger.Warn("memory allocation went negative, clamping to zero",
			"agent_id", agentID,
			"node_id", agent.NodeID,
			"computed_value", alloc.MemoryUsed+agent.MemoryBytes,
		)
		alloc.MemoryUsed = 0
	}
	alloc.VCPUsUsed -= agent.VCPUs
	if alloc.VCPUsUsed < 0 {
		s.logger.Warn("vCPU allocation went negative, clamping to zero",
			"agent_id", agentID,
			"node_id", agent.NodeID,
			"computed_value", alloc.VCPUsUsed+agent.VCPUs,
		)
		alloc.VCPUsUsed = 0
	}

	nodeID := agent.NodeID

	// Atomically clear the agent's node assignment and resource fields
	// in the store so that subsequent rebuildAllocations() calls do not
	// re-count the released resources. Holding s.mu during ModifyAgent
	// is safe: lock ordering is scheduler.mu -> store.mu.
	if err := s.store.ModifyAgent(agentID, func(a *state.AgentState) error {
		a.NodeID = ""
		a.MemoryBytes = 0
		a.VCPUs = 0
		return nil
	}); err != nil {
		s.logger.Warn("failed to clear agent node assignment in store after release",
			"agent_id", agentID,
			"error", err,
		)
	}

	// Mark allocations as dirty so the next Schedule call rebuilds from store,
	// ensuring consistency after the release.
	s.dirty = true

	s.logger.Info("released agent resources",
		"agent_id", agentID,
		"node_id", nodeID,
	)
	return nil
}

// resolveBackendType returns the backend type for the agent manifest.
func resolveBackendType(manifest *types.AgentManifest) string {
	if manifest.Spec.Runtime.Backend != "" {
		return manifest.Spec.Runtime.Backend
	}
	return "firecracker"
}

// validateNode checks whether a node is eligible for the given agent.
func (s *Scheduler) validateNode(node *types.NodeState, manifest *types.AgentManifest, memNeeded int64, vcpusNeeded int) error {
	// Reject nodes with invalid resource totals to prevent division by zero
	// in scoring calculations.
	if node.Resources.MemoryTotal <= 0 {
		return fmt.Errorf("node has invalid MemoryTotal: %d", node.Resources.MemoryTotal)
	}
	if node.Resources.MemoryTotal < 64*1024*1024 {
		return fmt.Errorf("node memory too low: %d bytes (minimum 64 MiB)", node.Resources.MemoryTotal)
	}
	if node.Resources.CPUCount <= 0 {
		return fmt.Errorf("node %s has no CPUs reported", node.ID)
	}

	backendType := resolveBackendType(manifest)

	// Only Firecracker requires Tier 1 and KVM.
	if backendType == "firecracker" {
		if node.Tier != types.NodeTier1 {
			return fmt.Errorf("not tier 1")
		}
		if !node.Resources.KVMAvail {
			return fmt.Errorf("KVM not available")
		}
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
	if availableMemory < 0 {
		availableMemory = 0
	}
	if memNeeded > availableMemory {
		return fmt.Errorf("insufficient memory: need %d, available %d", memNeeded, availableMemory)
	}
	availableVCPUs := node.Resources.CPUCount - alloc.VCPUsUsed
	if availableVCPUs < 0 {
		availableVCPUs = 0
	}
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
	if len(candidates) == 0 {
		return nil
	}

	const scoreEpsilon = 1e-9

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
		if loadRatio > maxLoadRatio+scoreEpsilon || (math.Abs(loadRatio-maxLoadRatio) <= scoreEpsilon && (mostLoadedID == "" || n.ID < mostLoadedID)) {
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

	scores := make([]scored, 0, len(candidates))
	for _, n := range candidates {
		alloc := s.getAlloc(n.ID)

		// Memory score: remaining memory after allocation / total.
		memRemaining := n.Resources.MemoryTotal - alloc.MemoryUsed - memNeeded
		if memRemaining < 0 {
			memRemaining = 0
		}
		memAfter := float64(memRemaining) / float64(n.Resources.MemoryTotal)

		// VCPU score: remaining vcpus after allocation / total.
		vcpuAfter := 0.0
		if n.Resources.CPUCount > 0 {
			cpuRemaining := n.Resources.CPUCount - alloc.VCPUsUsed - vcpusNeeded
			if cpuRemaining < 0 {
				cpuRemaining = 0
			}
			vcpuAfter = float64(cpuRemaining) / float64(n.Resources.CPUCount)
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
	// Use epsilon comparison for floating-point scores to avoid platform-
	// dependent ordering from insignificant rounding differences.
	sort.Slice(scores, func(i, j int) bool {
		diff := scores[i].score - scores[j].score
		if diff > scoreEpsilon {
			return true // i has clearly higher score
		}
		if diff < -scoreEpsilon {
			return false // j has clearly higher score
		}
		// Scores are effectively equal; tie-break alphabetically.
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
	if memBytes > 0 && alloc.MemoryUsed > math.MaxInt64-memBytes {
		s.logger.Warn("memory accumulation would overflow int64, capping at MaxInt64",
			"node_id", nodeID,
			"current", alloc.MemoryUsed,
			"adding", memBytes,
		)
		alloc.MemoryUsed = math.MaxInt64
	} else {
		alloc.MemoryUsed += memBytes
	}
	if vcpus > 0 && alloc.VCPUsUsed > math.MaxInt-vcpus {
		s.logger.Warn("vCPU accumulation would overflow int, capping at MaxInt",
			"node_id", nodeID,
			"current", alloc.VCPUsUsed,
			"adding", vcpus,
		)
		alloc.VCPUsUsed = math.MaxInt
	} else {
		alloc.VCPUsUsed += vcpus
	}
}

// defaultAgentMemoryBytes is the default memory allocation (512 MiB) used
// when an agent manifest does not specify a memory requirement. This must
// match the VM manager's default (internal/vm) to prevent under-accounting
// and resource over-commitment.
const defaultAgentMemoryBytes int64 = 512 * 1024 * 1024

// agentMemoryBytes parses the agent's memory requirement into bytes.
// Returns a default value (512 MiB) if no memory is specified, so that
// agents without explicit memory requirements still consume tracked
// resources and cannot silently bypass scheduler resource accounting.
func agentMemoryBytes(manifest *types.AgentManifest) (int64, error) {
	if manifest.Spec.Resources.Memory == "" {
		return defaultAgentMemoryBytes, nil
	}
	return config.ParseMemory(manifest.Spec.Resources.Memory)
}
