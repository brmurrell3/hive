package reconciler

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/hivehq/hive/internal/config"
	"github.com/hivehq/hive/internal/state"
	"github.com/hivehq/hive/internal/types"
)

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
	mu            sync.Mutex
	runMu         sync.Mutex // T2-07: serializes runOnce calls
	store         *state.Store
	clusterRoot   string
	logger        *slog.Logger
	handler       func(Action) error
	interval      time.Duration
	stopCh        chan struct{}
	stopped       chan struct{}
	stopOnce      sync.Once // T3-01: prevent double-close panic
	started       bool      // T3-02: prevent double-start
	triggerCh     chan struct{}
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
		triggerCh:   make(chan struct{}, 1), // T2-07: buffered for coalesced triggers
	}
}

// SetInterval overrides the default 5-second polling interval.
// Must be called before Start.
func (r *Reconciler) SetInterval(d time.Duration) {
	r.interval = d
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
// T3-02: Returns error if already started.
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
// T3-01: Safe to call multiple times.
func (r *Reconciler) Stop() {
	r.stopOnce.Do(func() {
		close(r.stopCh)
	})
	<-r.stopped
}

// Trigger forces an immediate reconciliation pass outside the timer.
// T2-07: Sends to a buffered channel, coalescing multiple triggers.
func (r *Reconciler) Trigger() {
	select {
	case r.triggerCh <- struct{}{}:
	default:
		// Already a pending trigger, no need to queue another.
	}
}

// runOnce performs a single reconciliation pass: compute actions and
// dispatch them to the handler. T2-07: Protected by runMu to prevent concurrent runs.
func (r *Reconciler) runOnce() {
	r.runMu.Lock()
	defer r.runMu.Unlock()

	actions := r.Reconcile()
	if len(actions) == 0 {
		return
	}

	r.mu.Lock()
	handler := r.handler
	r.mu.Unlock()

	if handler == nil {
		r.logger.Warn("reconciler has actions but no handler set", "count", len(actions))
		return
	}

	for _, action := range actions {
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
		// After a successful create or restart, stamp the ManifestHash onto the
		// agent state so that drift detection works on the next reconcile pass.
		if (action.Type == ActionCreate || action.Type == ActionRestart) && action.Manifest != nil {
			hash := ManifestHash(action.Manifest)
			if agentState := r.store.GetAgent(action.AgentID); agentState != nil {
				agentState.ManifestHash = hash
				if err := r.store.SetAgent(agentState); err != nil {
					r.logger.Error("reconciler: failed to persist manifest hash",
						"agent_id", action.AgentID,
						"error", err,
					)
				}
			}
		}
	}
}

// Reconcile compares desired state (loaded from cluster root manifests)
// with actual state (from the store) and returns a sorted list of actions
// needed to converge them.
//
// Actions are returned in a deterministic order:
//   - destroy actions first (sorted by agent ID)
//   - create actions next (sorted by agent ID)
//   - restart actions last (sorted by agent ID)
func (r *Reconciler) Reconcile() []Action {
	desired, err := config.LoadDesiredState(r.clusterRoot)
	if err != nil {
		r.logger.Error("reconciler: failed to load desired state", "error", err)
		return nil
	}

	actual := r.store.AllAgents()
	actualMap := make(map[string]*state.AgentState, len(actual))
	for _, a := range actual {
		actualMap[a.ID] = a
	}

	var destroyActions, createActions, restartActions []Action

	// Agents in desired but not in actual: need to create.
	for id, manifest := range desired.Agents {
		if _, exists := actualMap[id]; !exists {
			createActions = append(createActions, Action{
				Type:     ActionCreate,
				AgentID:  id,
				Manifest: manifest,
			})
		}
	}

	// Agents in actual but not in desired: need to destroy.
	for id, agentState := range actualMap {
		if _, exists := desired.Agents[id]; !exists {
			destroyActions = append(destroyActions, Action{
				Type:    ActionDestroy,
				AgentID: id,
				NodeID:  agentState.NodeID,
			})
		}
	}

	// Agents in both: check for manifest changes.
	for id, manifest := range desired.Agents {
		agentState, exists := actualMap[id]
		if !exists {
			continue
		}
		newHash, _ := manifestHash(manifest)
		if agentState.ManifestHash != "" && agentState.ManifestHash != newHash {
			restartActions = append(restartActions, Action{
				Type:     ActionRestart,
				AgentID:  id,
				NodeID:   agentState.NodeID,
				Manifest: manifest,
			})
		}
	}

	// Sort each category by agent ID for deterministic ordering.
	sortActions(destroyActions)
	sortActions(createActions)
	sortActions(restartActions)

	// Combine: destroy first, then create, then restart.
	var result []Action
	result = append(result, destroyActions...)
	result = append(result, createActions...)
	result = append(result, restartActions...)

	return result
}

// sortActions sorts a slice of actions by AgentID.
func sortActions(actions []Action) {
	sort.Slice(actions, func(i, j int) bool {
		return actions[i].AgentID < actions[j].AgentID
	})
}

// manifestHash computes a stable SHA-256 hash of the manifest.
// T2-08: Uses sorted map keys for deterministic output regardless of iteration order.
func manifestHash(manifest *types.AgentManifest) (string, error) {
	// Create a stable representation by sorting map keys.
	// We hash a canonical form: sort the struct's map fields.
	type stableManifest struct {
		APIVersion string
		Kind       string
		ID         string
		Team       string
		Labels     [][2]string // sorted key-value pairs
		Tier       string
		Mode       string
		Memory     string
		VCPUs      int
		Runtime    types.AgentRuntime
		Caps       []types.AgentCapability
		Env        [][2]string
		NodeLabels [][2]string
	}

	sortMap := func(m map[string]string) [][2]string {
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

	sm := stableManifest{
		APIVersion: manifest.APIVersion,
		Kind:       manifest.Kind,
		ID:         manifest.Metadata.ID,
		Team:       manifest.Metadata.Team,
		Labels:     sortMap(manifest.Metadata.Labels),
		Tier:       manifest.Spec.Tier,
		Mode:       manifest.Spec.Mode,
		Memory:     manifest.Spec.Resources.Memory,
		VCPUs:      manifest.Spec.Resources.VCPUs,
		Runtime:    manifest.Spec.Runtime,
		Caps:       manifest.Spec.Capabilities,
		Env:        sortMap(manifest.Spec.Runtime.Model.Env),
		NodeLabels: sortMap(manifest.Spec.Placement.NodeLabels),
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
func ManifestHash(manifest *types.AgentManifest) string {
	h, _ := manifestHash(manifest)
	return h
}
