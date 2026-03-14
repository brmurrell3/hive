// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

// Package reconciler continuously reconciles desired agent state against actual running state on a 5-second cycle.
package reconciler

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/brmurrell3/hive/internal/config"
	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/types"
)

var replicaSuffixPattern = regexp.MustCompile(`-\d+$`)

// AgentScheduler is the interface the reconciler uses to obtain node
// assignments for new agents.  Implementations should return the target
// node ID for the given agent manifest, or an error if no suitable node
// is available.
type AgentScheduler interface {
	Schedule(manifest *types.AgentManifest) (nodeID string, err error)
}

// ActionType describes the kind of reconciliation action to take.
type ActionType string

const (
	ActionCreate  ActionType = "create"
	ActionDestroy ActionType = "destroy"
	ActionRestart ActionType = "restart"
	ActionUpdate  ActionType = "update"
)

// Action represents a single reconciliation action to bring actual state
// in line with desired state.
type Action struct {
	Type     ActionType
	AgentID  string
	NodeID   string
	Manifest *types.AgentManifest
}

// Reconciler compares desired state (from cluster root manifests) with
// actual state (from the state store) and produces actions to converge them.
// It runs a periodic polling loop (every 5 seconds) and can also be
// triggered by fsnotify events.
type Reconciler struct {
	mu          sync.Mutex
	runMu       sync.Mutex // serializes runOnce calls
	store       *state.Store
	clusterRoot string
	logger      *slog.Logger
	handler     func(Action) error
	scheduler   AgentScheduler
	interval    time.Duration
	stopCh      chan struct{}
	stopped     chan struct{}
	stopOnce    sync.Once // prevent double-close panic
	started     bool      // prevent double-start
	triggerCh   chan struct{}

	recentDestroys []time.Time // sliding window of recent destruction timestamps
}

// NewReconciler creates a new Reconciler.
func NewReconciler(store *state.Store, clusterRoot string, logger *slog.Logger) *Reconciler {
	return &Reconciler{
		store:       store,
		clusterRoot: clusterRoot,
		logger:      logger,
		interval:    5 * time.Second,
		stopCh:      make(chan struct{}),
		stopped:     make(chan struct{}),
		triggerCh:   make(chan struct{}, 1), // buffered for coalesced triggers
	}
}

// IsRunning reports whether the reconciler's polling loop has been started
// and has not yet been stopped. It satisfies the dashboard.ReadyzChecker interface.
func (r *Reconciler) IsRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.started {
		return false
	}
	// Check if the stop channel has been closed.
	select {
	case <-r.stopCh:
		return false
	default:
		return true
	}
}

// SetInterval overrides the default 5-second polling interval.
// Must be called before Start; returns an error if called after Start.
func (r *Reconciler) SetInterval(d time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return fmt.Errorf("reconciler: SetInterval called after Start")
	}
	r.interval = d
	return nil
}

// SetScheduler configures the scheduler used to assign nodes when creating
// new agents.  If no scheduler is set (or set to nil), agents are created
// without a node assignment, which is the correct behavior for single-node
// deployments.
func (r *Reconciler) SetScheduler(s AgentScheduler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scheduler = s
}

// SetActionHandler sets the callback invoked for each reconciliation action.
// The handler is called synchronously during Reconcile; errors are logged
// but do not halt the loop.
func (r *Reconciler) SetActionHandler(handler func(Action) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handler = handler
}

// Start begins the periodic reconciliation loop. It runs in a goroutine
// and returns immediately. Call Stop to halt it.
// Returns error if already started.
func (r *Reconciler) Start() error {
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return fmt.Errorf("reconciler already started")
	}
	r.started = true
	r.mu.Unlock()

	r.logger.Info("reconciler starting", "interval", r.interval)

	go func() {
		defer close(r.stopped)

		// Run an initial reconciliation immediately.
		r.runOnce()

		ticker := time.NewTicker(r.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				r.runOnce()
			case <-r.triggerCh:
				r.runOnce()
			case <-r.stopCh:
				r.logger.Info("reconciler stopped")
				return
			}
		}
	}()

	return nil
}

// Stop halts the reconciliation loop and waits for it to finish.
// Safe to call multiple times. If Start() was never called, returns
// immediately without blocking.
func (r *Reconciler) Stop() {
	r.mu.Lock()
	wasStarted := r.started
	r.mu.Unlock()

	r.stopOnce.Do(func() {
		close(r.stopCh)
	})

	if !wasStarted {
		return
	}
	<-r.stopped
}

// Trigger forces an immediate reconciliation pass outside the timer.
// Sends to a buffered channel, coalescing multiple triggers.
func (r *Reconciler) Trigger() {
	select {
	case r.triggerCh <- struct{}{}:
	default:
		// Already a pending trigger, no need to queue another.
	}
}

// runOnce performs a single reconciliation pass: compute actions and
// dispatch them to the handler. Protected by runMu to prevent concurrent runs.
func (r *Reconciler) runOnce() {
	r.runMu.Lock()
	defer r.runMu.Unlock()

	actions := r.reconcileLocked()
	if len(actions) == 0 {
		return
	}

	r.mu.Lock()
	handler := r.handler
	sched := r.scheduler
	r.mu.Unlock()

	if handler == nil {
		r.logger.Warn("reconciler has actions but no handler set", "count", len(actions))
		return
	}

	for _, action := range actions {
		// For create actions, consult the scheduler to pick a target node.
		// If no scheduler is configured (single-node mode) or the scheduler
		// returns an error (e.g. no eligible nodes), we log and proceed
		// without a NodeID so single-node operation is unaffected.
		if action.Type == ActionCreate && sched != nil && action.Manifest != nil {
			nodeID, err := sched.Schedule(action.Manifest)
			if err != nil {
				r.logger.Warn("scheduler could not assign node, proceeding without assignment",
					"agent_id", action.AgentID,
					"error", err,
				)
			} else if nodeID != "" {
				action.NodeID = nodeID
				r.logger.Info("scheduler assigned node",
					"agent_id", action.AgentID,
					"node_id", nodeID,
				)
			}
		}

		r.logger.Info("reconciler action",
			"type", action.Type,
			"agent_id", action.AgentID,
			"node_id", action.NodeID,
		)
		if err := handler(action); err != nil {
			r.logger.Error("reconciler action failed",
				"type", action.Type,
				"agent_id", action.AgentID,
				"error", err,
			)
			continue
		}
		if action.Type == ActionDestroy {
			r.recentDestroys = append(r.recentDestroys, time.Now())
		}
		// After a successful create, restart, or update, stamp the ManifestHash
		// and SpecHash onto the agent state so that drift detection works on
		// the next reconcile pass.
		if (action.Type == ActionCreate || action.Type == ActionRestart || action.Type == ActionUpdate) && action.Manifest != nil {
			hash, hashErr := ManifestHash(action.Manifest)
			sHash, sHashErr := SpecHash(action.Manifest)
			if hashErr != nil {
				r.logger.Error("reconciler: failed to compute manifest hash",
					"agent_id", action.AgentID,
					"error", hashErr,
				)
			} else if err := r.store.ModifyAgent(action.AgentID, func(a *state.AgentState) error {
				a.ManifestHash = hash
				if sHashErr == nil {
					a.SpecHash = sHash
				}
				return nil
			}); err != nil {
				r.logger.Warn("failed to update manifest/spec hash",
					"agent", action.AgentID,
					"error", err,
				)
			}
		}
	}
}

// Reconcile compares desired state (loaded from cluster root manifests)
// with actual state (from the store) and returns a sorted list of actions
// needed to converge them.
//
// Reconcile is safe for concurrent use; it acquires runMu to serialize
// access to recentDestroys and other internal state.
//
// Actions are returned in a deterministic order:
//   - destroy actions first (sorted by agent ID)
//   - create actions next (sorted by agent ID)
//   - restart actions last (sorted by agent ID)
func (r *Reconciler) Reconcile() []Action {
	r.runMu.Lock()
	defer r.runMu.Unlock()
	return r.reconcileLocked()
}

// reconcileLocked is the internal implementation of Reconcile. Callers must
// hold runMu.
func (r *Reconciler) reconcileLocked() []Action {
	desired, err := config.LoadDesiredState(r.clusterRoot)
	if err != nil {
		r.logger.Error("reconciler: failed to load desired state", "error", err)
		return nil
	}

	// Expand agents with replicas.min > 1 into individual instances.
	desiredAgents := r.expandReplicas(desired.Agents)

	actual := r.store.AllAgents()
	actualMap := make(map[string]*state.AgentState, len(actual))
	for _, a := range actual {
		actualMap[a.ID] = a
	}

	// Safeguard: if LoadDesiredState returns 0 agents but the store has
	// running agents, this likely indicates a configuration error (e.g.,
	// empty or missing manifests directory). Log a warning and skip
	// destructive actions to prevent accidentally destroying all agents.
	if len(desiredAgents) == 0 && len(actual) > 0 {
		r.logger.Warn("reconciler: desired state has 0 agents but store has running agents; "+
			"skipping reconciliation to prevent mass destruction",
			"actual_agents", len(actual),
		)
		return nil
	}

	var destroyActions, createActions, restartActions, updateActions []Action

	// Agents in desired but not in actual: need to create.
	for id, manifest := range desiredAgents {
		if _, exists := actualMap[id]; !exists {
			// Skip external agents — they join via hive-agent, not managed by hived.
			if !manifest.Spec.IsManaged() {
				continue
			}
			createActions = append(createActions, Action{
				Type:     ActionCreate,
				AgentID:  id,
				Manifest: manifest,
			})
		}
	}

	// Agents in actual but not in desired: need to destroy.
	for id, agentState := range actualMap {
		if _, exists := desiredAgents[id]; !exists {
			destroyActions = append(destroyActions, Action{
				Type:    ActionDestroy,
				AgentID: id,
				NodeID:  agentState.NodeID,
			})
		}
	}

	// Safeguard: if more than 50% of running agents would be destroyed,
	// log a warning and skip destroy actions. This prevents accidental
	// mass destruction from partial manifest directory changes.
	// Single-agent clusters are exempt since removing the only agent is
	// a valid operation and should not be blocked.
	if len(actual) > 1 && len(destroyActions) > len(actual)/2 {
		r.logger.Warn("reconciler: mass destruction safeguard triggered; "+
			"more than 50% of agents would be destroyed, skipping destroy actions",
			"destroy_count", len(destroyActions),
			"actual_agents", len(actual),
		)
		destroyActions = nil
	}

	// Sliding-window rate limit: if more than 50% of the fleet has been
	// destroyed in the last 60 seconds (including pending destroys), halt
	// further destruction. This prevents the percentage safeguard above
	// from being bypassed by incremental removals across reconcile passes.
	if len(destroyActions) > 0 && len(actual) > 1 {
		now := time.Now()
		windowStart := now.Add(-60 * time.Second)
		// Prune expired entries.
		valid := r.recentDestroys[:0]
		for _, t := range r.recentDestroys {
			if t.After(windowStart) {
				valid = append(valid, t)
			}
		}
		r.recentDestroys = valid

		totalInWindow := len(r.recentDestroys) + len(destroyActions)
		if totalInWindow > len(actual)/2 {
			r.logger.Warn("reconciler: sliding-window destruction rate limit triggered; "+
				"too many agents destroyed in the last 60s, skipping destroy actions",
				"recent_destroys", len(r.recentDestroys),
				"pending_destroys", len(destroyActions),
				"actual_agents", len(actual),
			)
			destroyActions = nil
		}
	}

	// Agents in both: check for manifest changes or FAILED status.
	for id, manifest := range desiredAgents {
		agentState, exists := actualMap[id]
		if !exists {
			continue
		}

		// Skip external agents — they are managed by hive-agent join and their
		// own systemd service, not by hived's reconciler.
		if !manifest.Spec.IsManaged() {
			continue
		}

		// Auto-recover FAILED agents regardless of manifest hash state.
		// A FAILED agent that is still desired should be restarted to
		// attempt recovery, even if its ManifestHash was never set.
		if agentState.Status == state.AgentStatusFailed {
			r.logger.Info("reconciler: auto-recovering FAILED agent",
				"agent_id", id,
			)
			restartActions = append(restartActions, Action{
				Type:     ActionRestart,
				AgentID:  id,
				NodeID:   agentState.NodeID,
				Manifest: manifest,
			})
			continue
		}

		newHash, err := manifestHash(manifest)
		if err != nil {
			r.logger.Warn("reconciler: failed to hash manifest, skipping drift check",
				"agent_id", id,
				"error", err,
			)
			continue
		}
		if agentState.ManifestHash != "" && agentState.ManifestHash != newHash {
			// Determine whether this is a restart-worthy change or a
			// lightweight update (e.g., labels/metadata only).
			if isLightweightChange(manifest, agentState) {
				updateActions = append(updateActions, Action{
					Type:     ActionUpdate,
					AgentID:  id,
					NodeID:   agentState.NodeID,
					Manifest: manifest,
				})
			} else {
				restartActions = append(restartActions, Action{
					Type:     ActionRestart,
					AgentID:  id,
					NodeID:   agentState.NodeID,
					Manifest: manifest,
				})
			}
		}
	}

	// Sort each category by agent ID for deterministic ordering.
	sortActions(destroyActions)
	sortActions(createActions)
	sortActions(restartActions)
	sortActions(updateActions)

	// Combine: destroy first, then create, then update, then restart.
	var result []Action
	result = append(result, destroyActions...)
	result = append(result, createActions...)
	result = append(result, updateActions...)
	result = append(result, restartActions...)

	return result
}

// maxReplicaCount is the upper bound on Replicas.Min to prevent runaway
// replica expansion from misconfigured manifests.
const maxReplicaCount = 1000

// expandReplicas takes the desired agents map and expands agents with replicas
// into individual instance entries (e.g., "worker" with min=3 becomes
// "worker-0", "worker-1", "worker-2"). After expansion, it checks for
// collisions between replica IDs and non-replica agent IDs (e.g., agent
// "worker-0" colliding with replica 0 of agent "worker"). Colliding
// replicas are skipped with a warning.
func (r *Reconciler) expandReplicas(agents map[string]*types.AgentManifest) map[string]*types.AgentManifest {
	// First pass: collect non-replica agent IDs and build a complete set of
	// all generated replica IDs so we can detect collisions in both directions.
	nonReplicaIDs := make(map[string]bool)
	type replicaEntry struct {
		parentID string
	}
	allReplicaIDs := make(map[string]replicaEntry)

	for id, manifest := range agents {
		if manifest.Spec.Replicas.Min <= 1 {
			if replicaSuffixPattern.MatchString(id) {
				r.logger.Warn("reconciler: agent ID resembles a replica ID (ends with -<number>), may cause ambiguity",
					"agent_id", id,
				)
			}
			nonReplicaIDs[id] = true
		} else {
			count := manifest.Spec.Replicas.Min
			if count > maxReplicaCount {
				count = maxReplicaCount
			}
			for i := 0; i < count; i++ {
				replicaID := fmt.Sprintf("%s-%d", id, i)
				if prev, exists := allReplicaIDs[replicaID]; exists {
					r.logger.Warn("reconciler: replica ID collision between two replica sets, skipping duplicate",
						"replica_id", replicaID,
						"parent_agent_1", prev.parentID,
						"parent_agent_2", id,
					)
					continue
				}
				allReplicaIDs[replicaID] = replicaEntry{parentID: id}
			}
		}
	}

	expanded := make(map[string]*types.AgentManifest)
	for id, manifest := range agents {
		replicaCount := manifest.Spec.Replicas.Min
		if replicaCount <= 1 {
			// Check if this non-replica agent's ID collides with a generated replica ID.
			if entry, collision := allReplicaIDs[id]; collision {
				r.logger.Warn("reconciler: standalone agent ID collides with a replica ID, skipping standalone",
					"agent_id", id,
					"colliding_parent", entry.parentID,
				)
				continue
			}
			expanded[id] = deepCopyManifest(manifest)
			continue
		}
		if replicaCount > maxReplicaCount {
			r.logger.Warn("reconciler: replicas.min exceeds maximum, capping",
				"agent_id", id,
				"requested", replicaCount,
				"capped_to", maxReplicaCount,
			)
			replicaCount = maxReplicaCount
		}
		for i := 0; i < replicaCount; i++ {
			replicaID := fmt.Sprintf("%s-%d", id, i)
			if nonReplicaIDs[replicaID] {
				r.logger.Warn("reconciler: replica ID collides with standalone agent ID, skipping replica",
					"replica_id", replicaID,
					"parent_agent", id,
				)
				continue
			}
			replica := deepCopyManifest(manifest)
			replica.Metadata.ID = replicaID
			expanded[replicaID] = replica
		}
	}
	return expanded
}

// deepCopyManifest returns a fully independent copy of the manifest,
// deep-copying all map and slice fields so that mutations to the copy
// do not affect the original (or other copies).
// Note: If AgentIngress gains reference-type fields, add deep copy logic here.
func deepCopyManifest(src *types.AgentManifest) *types.AgentManifest {
	dst := *src

	// Metadata
	dst.Metadata = src.Metadata
	dst.Metadata.Labels = copyMapSS(src.Metadata.Labels)

	// Spec - copy all slice/map fields
	dst.Spec = src.Spec

	// Deep-copy *bool fields in Spec to avoid aliasing between copies.
	if src.Spec.Health.Enabled != nil {
		enabled := *src.Spec.Health.Enabled
		dst.Spec.Health.Enabled = &enabled
	}

	// Capabilities
	if src.Spec.Capabilities != nil {
		dst.Spec.Capabilities = make([]types.AgentCapability, len(src.Spec.Capabilities))
		for i, cap := range src.Spec.Capabilities {
			dst.Spec.Capabilities[i] = cap
			if cap.Inputs != nil {
				dst.Spec.Capabilities[i].Inputs = make([]types.CapabilityParam, len(cap.Inputs))
				copy(dst.Spec.Capabilities[i].Inputs, cap.Inputs)
				// CapabilityParam contains a *bool (Required) pointer field.
				// The copy() above is a shallow copy that shares the pointer,
				// so we must deep copy it to avoid aliasing between replicas.
				for j, p := range cap.Inputs {
					if p.Required != nil {
						req := *p.Required
						dst.Spec.Capabilities[i].Inputs[j].Required = &req
					}
				}
			}
			if cap.Outputs != nil {
				dst.Spec.Capabilities[i].Outputs = make([]types.CapabilityParam, len(cap.Outputs))
				copy(dst.Spec.Capabilities[i].Outputs, cap.Outputs)
				// Same deep copy for the *bool Required field in Outputs.
				for j, p := range cap.Outputs {
					if p.Required != nil {
						req := *p.Required
						dst.Spec.Capabilities[i].Outputs[j].Required = &req
					}
				}
			}
		}
	}

	// Runtime.Model.Env
	dst.Spec.Runtime.Model.Env = copyMapSS(src.Spec.Runtime.Model.Env)

	// Network.EgressAllowlist
	if src.Spec.Network.EgressAllowlist != nil {
		dst.Spec.Network.EgressAllowlist = make([]string, len(src.Spec.Network.EgressAllowlist))
		copy(dst.Spec.Network.EgressAllowlist, src.Spec.Network.EgressAllowlist)
	}

	// Volumes
	if src.Spec.Volumes != nil {
		dst.Spec.Volumes = make([]types.AgentVolume, len(src.Spec.Volumes))
		copy(dst.Spec.Volumes, src.Spec.Volumes)
	}

	// Mounts
	if src.Spec.Mounts != nil {
		dst.Spec.Mounts = make([]types.AgentMount, len(src.Spec.Mounts))
		copy(dst.Spec.Mounts, src.Spec.Mounts)
	}

	// Secrets
	if src.Spec.Secrets != nil {
		dst.Spec.Secrets = make([]types.AgentSecret, len(src.Spec.Secrets))
		copy(dst.Spec.Secrets, src.Spec.Secrets)
	}

	// Placement.NodeLabels
	dst.Spec.Placement.NodeLabels = copyMapSS(src.Spec.Placement.NodeLabels)

	// Hardware slices and map
	if src.Spec.Hardware.Sensors != nil {
		dst.Spec.Hardware.Sensors = make([]string, len(src.Spec.Hardware.Sensors))
		copy(dst.Spec.Hardware.Sensors, src.Spec.Hardware.Sensors)
	}
	if src.Spec.Hardware.Actuators != nil {
		dst.Spec.Hardware.Actuators = make([]string, len(src.Spec.Hardware.Actuators))
		copy(dst.Spec.Hardware.Actuators, src.Spec.Hardware.Actuators)
	}
	dst.Spec.Hardware.Custom = copyMapSS(src.Spec.Hardware.Custom)

	return &dst
}

// copyMapSS returns a deep copy of a map[string]string, or nil if the input is nil.
func copyMapSS(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	cp := make(map[string]string, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

// sortActions sorts a slice of actions by AgentID.
func sortActions(actions []Action) {
	sort.Slice(actions, func(i, j int) bool {
		return actions[i].AgentID < actions[j].AgentID
	})
}

// sortMapPairs converts a map[string]string into a deterministically ordered
// [][2]string by sorting on keys. Returns nil for empty/nil maps. This
// ensures stable JSON serialization for hash computation.
func sortMapPairs(m map[string]string) [][2]string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([][2]string, len(keys))
	for i, k := range keys {
		pairs[i] = [2]string{k, m[k]}
	}
	return pairs
}

// stableSpec is the canonical representation of an agent's spec-level fields
// used for deterministic hash computation. Map fields are replaced with sorted
// [][2]string pairs, and capabilities and egress allowlists are sorted by name.
//
// IMPORTANT: Hash stability depends on deterministic JSON serialization.
// The field order in this struct determines the JSON field order, which in turn
// determines the hash output. Do NOT reorder, rename, or remove fields without
// understanding that this will change all computed hashes and trigger spurious
// reconciliation restarts.
//
// If any embedded types (e.g., AgentNetwork, AgentHealth, AgentRestart,
// AgentReplicas, AgentIngress) gain map fields in the future, those maps MUST
// be broken out and sorted the same way, otherwise Go's non-deterministic map
// iteration will produce unstable hashes.
type stableSpec struct {
	// Spec top-level
	Tier string
	Mode string

	// Resources (all fields)
	Memory string
	VCPUs  int
	Disk   string

	// Runtime - broken out so we can sort the Model.Env map
	RuntimeType    string
	RuntimeBackend string
	RuntimeCommand string
	RuntimeImage   string
	ModelProvider  string
	ModelName      string
	Env            [][2]string // sorted Runtime.Model.Env

	// Capabilities
	Caps []types.AgentCapability

	// Network
	Network types.AgentNetwork

	// Volumes
	Volumes []types.AgentVolume

	// Mounts
	Mounts []types.AgentMount

	// Secrets
	Secrets []types.AgentSecret

	// Replicas
	Replicas types.AgentReplicas

	// Health
	Health types.AgentHealth

	// Restart
	Restart types.AgentRestart

	// Placement - broken out so we can sort NodeLabels map
	PlacementNodeID string
	PlacementArch   string
	NodeLabels      [][2]string

	// Hardware - broken out so we can sort Custom map
	HardwareGPIO      bool
	HardwareCamera    bool
	HardwareSensors   []string
	HardwareActuators []string
	HardwareGPU       string
	HardwareCustom    [][2]string

	// Ingress
	Ingress types.AgentIngress
}

// buildStableSpec extracts spec-level fields from a manifest into a stableSpec,
// sorting all map fields and capabilities for deterministic serialization.
func buildStableSpec(manifest *types.AgentManifest) stableSpec {
	// Sort capabilities by name for deterministic hashing regardless of
	// declaration order in the manifest file.
	sortedCaps := make([]types.AgentCapability, len(manifest.Spec.Capabilities))
	copy(sortedCaps, manifest.Spec.Capabilities)
	sort.Slice(sortedCaps, func(i, j int) bool {
		return sortedCaps[i].Name < sortedCaps[j].Name
	})

	// Copy Network and sort EgressAllowlist for deterministic hashing
	// regardless of declaration order in the manifest file.
	network := manifest.Spec.Network
	if len(network.EgressAllowlist) > 0 {
		sorted := make([]string, len(network.EgressAllowlist))
		copy(sorted, network.EgressAllowlist)
		sort.Strings(sorted)
		network.EgressAllowlist = sorted
	}

	return stableSpec{
		Tier:   manifest.Spec.Tier,
		Mode:   manifest.Spec.Mode,
		Memory: manifest.Spec.Resources.Memory,
		VCPUs:  manifest.Spec.Resources.VCPUs,
		Disk:   manifest.Spec.Resources.Disk,

		RuntimeType:    manifest.Spec.Runtime.Type,
		RuntimeBackend: manifest.Spec.Runtime.Backend,
		RuntimeCommand: manifest.Spec.Runtime.Command,
		RuntimeImage:   manifest.Spec.Runtime.Image,
		ModelProvider:  manifest.Spec.Runtime.Model.Provider,
		ModelName:      manifest.Spec.Runtime.Model.Name,
		Env:            sortMapPairs(manifest.Spec.Runtime.Model.Env),

		Caps:    sortedCaps,
		Network: network,
		Volumes: manifest.Spec.Volumes,
		Mounts:  manifest.Spec.Mounts,
		Secrets: manifest.Spec.Secrets,

		Replicas: manifest.Spec.Replicas,
		Health:   manifest.Spec.Health,
		Restart:  manifest.Spec.Restart,

		PlacementNodeID: manifest.Spec.Placement.NodeID,
		PlacementArch:   manifest.Spec.Placement.Arch,
		NodeLabels:      sortMapPairs(manifest.Spec.Placement.NodeLabels),

		HardwareGPIO:      manifest.Spec.Hardware.GPIO,
		HardwareCamera:    manifest.Spec.Hardware.Camera,
		HardwareSensors:   manifest.Spec.Hardware.Sensors,
		HardwareActuators: manifest.Spec.Hardware.Actuators,
		HardwareGPU:       manifest.Spec.Hardware.GPU,
		HardwareCustom:    sortMapPairs(manifest.Spec.Hardware.Custom),

		Ingress: manifest.Spec.Ingress,
	}
}

// manifestHash computes a stable SHA-256 hash of the manifest.
// Uses sorted map keys for deterministic output regardless of iteration order.
// All fields from AgentManifest/AgentSpec are included so that any change
// triggers drift detection.
//
// The stableManifest struct embeds stableSpec (anonymously) so that its fields
// are promoted and serialized flat — producing the same JSON layout as the
// original inline struct definition. The metadata fields (APIVersion, Kind,
// ID, Team, Labels) appear first, followed by all spec fields from stableSpec.
func manifestHash(manifest *types.AgentManifest) (string, error) {
	// stableManifest wraps stableSpec with top-level and metadata fields.
	// The anonymous embed of stableSpec causes json.Marshal to promote
	// its fields inline, preserving the original flat JSON structure.
	type stableManifest struct {
		// Top-level
		APIVersion string
		Kind       string

		// Metadata
		ID     string
		Team   string
		Labels [][2]string // sorted key-value pairs

		// Spec fields (promoted from embedded stableSpec)
		stableSpec
	}

	sm := stableManifest{
		APIVersion: manifest.APIVersion,
		Kind:       manifest.Kind,
		ID:         manifest.Metadata.ID,
		Team:       manifest.Metadata.Team,
		Labels:     sortMapPairs(manifest.Metadata.Labels),
		stableSpec: buildStableSpec(manifest),
	}

	data, err := json.Marshal(sm)
	if err != nil {
		return "", fmt.Errorf("marshaling manifest for hash: %w", err)
	}
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h), nil
}

// ManifestHash is exported for use by other packages that need to compute
// the hash of a manifest (e.g., when setting ManifestHash on AgentState).
// Returns the hash and any error from marshaling.
func ManifestHash(manifest *types.AgentManifest) (string, error) {
	return manifestHash(manifest)
}

// isLightweightChange returns true if the manifest change only affects
// metadata fields (labels, team, description) that do not require a full
// restart. It compares the spec-only hash of the new manifest against the
// stored SpecHash on the agent state. If the spec hashes match but the
// full manifest hashes differ, only metadata changed.
func isLightweightChange(manifest *types.AgentManifest, agentState *state.AgentState) bool {
	if agentState.SpecHash == "" {
		// No stored spec hash (legacy agent or first reconciliation).
		// Cannot determine if change is lightweight, so assume it requires restart.
		return false
	}

	newSpecHash, err := specHash(manifest)
	if err != nil {
		return false
	}

	// If the spec hash has not changed, the modification was metadata-only.
	return newSpecHash == agentState.SpecHash
}

// specHash computes a stable SHA-256 hash of only the spec (runtime-affecting)
// fields of a manifest, excluding metadata-only fields like labels, team, and
// the agent ID. This allows detecting whether a manifest change requires a
// restart or is just a metadata update.
func specHash(manifest *types.AgentManifest) (string, error) {
	ss := buildStableSpec(manifest)

	data, err := json.Marshal(ss)
	if err != nil {
		return "", fmt.Errorf("marshaling spec for hash: %w", err)
	}
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h), nil
}

// SpecHash is exported for use by other packages that need to compute the
// spec-only hash of a manifest (e.g., when setting SpecHash on AgentState).
func SpecHash(manifest *types.AgentManifest) (string, error) {
	return specHash(manifest)
}
