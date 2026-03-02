// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package health

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/types"
)

const maxBackoff = 5 * time.Minute

// VMManager is the interface required for restarting agent VMs.
// It is satisfied by *vm.Manager but defined here to avoid an import cycle.
type VMManager interface {
	StopAgent(ctx context.Context, agentID string) error
	StartAgent(ctx context.Context, agent *types.AgentManifest) error
}

// RestartConfig defines the restart policy for a single agent.
type RestartConfig struct {
	Policy      string // "always", "on-failure", "never"
	MaxRestarts int
	Backoff     time.Duration
	Manifest    *types.AgentManifest
}

// RestartManager enforces restart policies for unhealthy agents. When the
// health monitor detects an unhealthy agent it delegates to HandleUnhealthy
// which consults the configured policy and, if appropriate, orchestrates a
// stop-then-start cycle via the VMManager.
type RestartManager struct {
	store     *state.Store
	vmMgr     VMManager
	logger    *slog.Logger
	configs   map[string]RestartConfig // agentID -> config
	onRestart func(agentID string)     // called after a successful restart
	mu        sync.Mutex
	active    bool // set to true on first HandleUnhealthyCtx call
}

// NewRestartManager creates a RestartManager that uses the given state store
// and VM manager to inspect agent state and perform restarts.
func NewRestartManager(store *state.Store, vmMgr VMManager, logger *slog.Logger) *RestartManager {
	return &RestartManager{
		store:   store,
		vmMgr:   vmMgr,
		logger:  logger.With("component", "restart-manager"),
		configs: make(map[string]RestartConfig),
	}
}

// SetOnRestart registers a callback that is invoked after a successful agent
// restart. The health monitor uses this to reset the agent's heartbeat timer
// so the newly restarted agent has a grace period to send its first heartbeat.
func (rm *RestartManager) SetOnRestart(fn func(agentID string)) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	if rm.active {
		rm.logger.Warn("SetOnRestart called after restart manager is already active; in-flight restarts may use the old callback")
	}
	rm.onRestart = fn
}

// SetConfig registers (or updates) the restart policy for the given agent.
// The manifest is deep-copied to prevent the caller from mutating it after registration.
func (rm *RestartManager) SetConfig(agentID string, cfg RestartConfig) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	if cfg.Manifest != nil {
		cp := *cfg.Manifest
		cp.Spec.Capabilities = append([]types.AgentCapability(nil), cfg.Manifest.Spec.Capabilities...)
		cp.Spec.Volumes = append([]types.AgentVolume(nil), cfg.Manifest.Spec.Volumes...)
		cp.Spec.Mounts = append([]types.AgentMount(nil), cfg.Manifest.Spec.Mounts...)
		cp.Spec.Secrets = append([]types.AgentSecret(nil), cfg.Manifest.Spec.Secrets...)
		if cfg.Manifest.Metadata.Labels != nil {
			cp.Metadata.Labels = make(map[string]string, len(cfg.Manifest.Metadata.Labels))
			for k, v := range cfg.Manifest.Metadata.Labels {
				cp.Metadata.Labels[k] = v
			}
		}
		cfg.Manifest = &cp
	}
	rm.configs[agentID] = cfg
}

// HandleUnhealthy is called when an agent is detected as unhealthy. It
// evaluates the configured restart policy and either restarts the agent or
// transitions it to the FAILED state.
//
// Deprecated: Use HandleUnhealthyCtx. Kept for backward compatibility.
func (rm *RestartManager) HandleUnhealthy(agentID string) error {
	return rm.HandleUnhealthyCtx(context.Background(), agentID)
}

// HandleUnhealthyCtx is called when an agent is detected as unhealthy. It
// evaluates the configured restart policy and either restarts the agent or
// transitions it to the FAILED state. The context allows callers to cancel
// pending backoff sleeps (e.g., on shutdown).
func (rm *RestartManager) HandleUnhealthyCtx(ctx context.Context, agentID string) error {
	rm.mu.Lock()
	cfg, ok := rm.configs[agentID]
	rm.active = true
	rm.mu.Unlock()

	if !ok {
		rm.logger.Warn("no restart config for agent, treating as policy=never", "agent_id", agentID)
		return nil
	}

	agentState := rm.store.GetAgent(agentID)
	if agentState == nil {
		return fmt.Errorf("agent %s not found in state store", agentID)
	}

	switch cfg.Policy {
	case "never":
		rm.logger.Info("restart policy is never, skipping restart", "agent_id", agentID)
		return nil

	case "on-failure":
		// "on-failure" restarts only when the agent was RUNNING (implying
		// an unexpected crash/heartbeat loss) or already FAILED.
		if agentState.Status != state.AgentStatusRunning && agentState.Status != state.AgentStatusFailed {
			rm.logger.Info("on-failure policy: agent not in restartable state",
				"agent_id", agentID,
				"status", agentState.Status,
			)
			return nil
		}

	case "always":
		// "always" proceeds regardless of current state.

	default:
		return fmt.Errorf("unknown restart policy %q for agent %s", cfg.Policy, agentID)
	}

	// Apply exponential backoff before attempting the restart.
	// backoff = base * 2^min(restartCount, 5), capped at maxBackoff.
	if cfg.Backoff > 0 {
		exp := agentState.RestartCount
		if exp > 5 {
			exp = 5
		}
		backoff := cfg.Backoff * time.Duration(1<<uint(exp))
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
		rm.logger.Info("applying exponential restart backoff",
			"agent_id", agentID,
			"backoff", backoff,
			"base_backoff", cfg.Backoff,
			"restart_count", agentState.RestartCount,
		)
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
			// Backoff elapsed normally.
			timer.Stop() // For GC cleanliness: release timer resources.
		case <-ctx.Done():
			timer.Stop()
			rm.logger.Info("restart backoff cancelled",
				"agent_id", agentID,
				"error", ctx.Err(),
			)
			return ctx.Err()
		}

		// Re-read agent state after backoff; it may have been removed.
		agentState = rm.store.GetAgent(agentID)
		if agentState == nil {
			return fmt.Errorf("agent %s removed during restart backoff", agentID)
		}
		if agentState.Status != state.AgentStatusFailed {
			rm.logger.Info("agent no longer in FAILED state after backoff, skipping restart",
				"agent_id", agentID,
				"status", agentState.Status,
			)
			return nil
		}

		// Re-read config after backoff; it may have been updated.
		rm.mu.Lock()
		if updated, ok := rm.configs[agentID]; ok {
			cfg = updated
		}
		rm.mu.Unlock()
	}

	// Nil check for manifest before attempting restart state transitions.
	if cfg.Manifest == nil {
		return fmt.Errorf("restart config has nil manifest for agent %s", agentID)
	}

	// Atomically check MaxRestarts and increment restart count inside
	// ModifyAgent to prevent race conditions with concurrent callers.
	// MaxRestarts is checked atomically inside ModifyAgent below.
	var exceededMax bool
	var newRestartCount int
	if err := rm.store.ModifyAgent(agentID, func(a *state.AgentState) error {
		if cfg.MaxRestarts > 0 && a.RestartCount >= cfg.MaxRestarts {
			a.Status = state.AgentStatusFailed
			a.Error = fmt.Sprintf("exceeded max restarts (%d/%d)", a.RestartCount, cfg.MaxRestarts)
			a.LastTransition = time.Now()
			exceededMax = true
			return nil // not an error, just a state transition
		}
		a.RestartCount++
		a.Status = state.AgentStatusCreating
		a.Error = ""
		a.LastTransition = time.Now()
		newRestartCount = a.RestartCount
		return nil
	}); err != nil {
		return fmt.Errorf("updating agent state for restart of %s: %w", agentID, err)
	}

	if exceededMax {
		rm.logger.Warn("agent exceeded max restarts, transitioning to FAILED",
			"agent_id", agentID,
			"max_restarts", cfg.MaxRestarts,
		)
		return nil
	}

	rm.logger.Info("restarting unhealthy agent",
		"agent_id", agentID,
		"policy", cfg.Policy,
		"restart_count", newRestartCount,
	)

	// Stop the agent first (best-effort; it may already be dead).
	if err := rm.vmMgr.StopAgent(ctx, agentID); err != nil {
		rm.logger.Warn("error stopping agent before restart, continuing",
			"agent_id", agentID,
			"error", err,
		)
	}

	// Start the agent with the manifest from the config.
	if err := rm.vmMgr.StartAgent(ctx, cfg.Manifest); err != nil {
		// Revert agent status to FAILED since StartAgent did not succeed.
		if modErr := rm.store.ModifyAgent(agentID, func(a *state.AgentState) error {
			a.Status = state.AgentStatusFailed
			a.Error = fmt.Sprintf("restart failed: %v", err)
			a.LastTransition = time.Now()
			return nil
		}); modErr != nil {
			rm.logger.Error("failed to revert agent status after StartAgent failure",
				"agent_id", agentID,
				"error", modErr,
			)
		}
		return fmt.Errorf("restarting agent %s: %w", agentID, err)
	}

	rm.logger.Info("agent restarted successfully",
		"agent_id", agentID,
		"restart_count", newRestartCount,
	)

	// Notify the monitor so it can reset the heartbeat timer, giving the
	// newly restarted agent a fresh window to send its first heartbeat.
	rm.mu.Lock()
	cb := rm.onRestart
	rm.mu.Unlock()
	if cb != nil {
		cb(agentID)
	}

	return nil
}
