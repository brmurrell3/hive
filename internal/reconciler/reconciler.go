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
	store         *state.Store
	clusterRoot   string
	logger        *slog.Logger
	handler       func(Action) error
	interval      time.Duration
	stopCh        chan struct{}
	stopped       chan struct{}
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
func (r *Reconciler) Start() error {
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
			case <-r.stopCh:
				r.logger.Info("reconciler stopped")
				return
			}
		}
	}()

	return nil
}

// Stop halts the reconciliation loop and waits for it to finish.
func (r *Reconciler) Stop() {
	close(r.stopCh)
	<-r.stopped
}

// Trigger forces an immediate reconciliation pass outside the timer.
// This is intended to be called from fsnotify event handlers.
func (r *Reconciler) Trigger() {
	r.runOnce()
}

// runOnce performs a single reconciliation pass: compute actions and
// dispatch them to the handler.
func (r *Reconciler) runOnce() {
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
		newHash := manifestHash(manifest)
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

// manifestHash computes a SHA-256 hash of the manifest's JSON representation
// to detect changes.
func manifestHash(manifest *types.AgentManifest) string {
	data, err := json.Marshal(manifest)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h)
}

// ManifestHash is exported for use by other packages that need to compute
// the hash of a manifest (e.g., when setting ManifestHash on AgentState).
func ManifestHash(manifest *types.AgentManifest) string {
	return manifestHash(manifest)
}
