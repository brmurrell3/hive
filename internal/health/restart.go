package health

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/types"
)

// VMManager is the interface required for restarting agent VMs.
// It is satisfied by *vm.Manager but defined here to avoid an import cycle.
type VMManager interface {
	StopAgent(agentID string) error
	StartAgent(agent *types.AgentManifest) error
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
	store   *state.Store
	vmMgr   VMManager
	logger  *slog.Logger
	configs map[string]RestartConfig // agentID -> config
	mu      sync.Mutex
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

// SetConfig registers (or updates) the restart policy for the given agent.
func (rm *RestartManager) SetConfig(agentID string, cfg RestartConfig) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	rm.configs[agentID] = cfg
}

// HandleUnhealthy is called when an agent is detected as unhealthy. It
// evaluates the configured restart policy and either restarts the agent or
// transitions it to the FAILED state.
func (rm *RestartManager) HandleUnhealthy(agentID string) error {
	rm.mu.Lock()
	cfg, ok := rm.configs[agentID]
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

	// Check whether the maximum number of restarts has been exceeded.
	if cfg.MaxRestarts > 0 && agentState.RestartCount >= cfg.MaxRestarts {
		rm.logger.Warn("agent exceeded max restarts, transitioning to FAILED",
			"agent_id", agentID,
			"restart_count", agentState.RestartCount,
			"max_restarts", cfg.MaxRestarts,
		)

		agentState.Status = state.AgentStatusFailed
		agentState.Error = fmt.Sprintf("exceeded max restarts (%d/%d)", agentState.RestartCount, cfg.MaxRestarts)
		agentState.LastTransition = time.Now()
		if err := rm.store.SetAgent(agentState); err != nil {
			return fmt.Errorf("persisting FAILED state for agent %s: %w", agentID, err)
		}
		return nil
	}

	// Apply backoff before attempting the restart.
	if cfg.Backoff > 0 {
		rm.logger.Info("applying restart backoff",
			"agent_id", agentID,
			"backoff", cfg.Backoff,
			"restart_count", agentState.RestartCount,
		)
		time.Sleep(cfg.Backoff)
	}

	rm.logger.Info("restarting unhealthy agent",
		"agent_id", agentID,
		"policy", cfg.Policy,
		"restart_count", agentState.RestartCount+1,
	)

	// Stop the agent first (best-effort; it may already be dead).
	if err := rm.vmMgr.StopAgent(agentID); err != nil {
		rm.logger.Warn("error stopping agent before restart, continuing",
			"agent_id", agentID,
			"error", err,
		)
	}

	// Increment restart count and persist before starting.
	agentState.RestartCount++
	agentState.Error = ""
	agentState.LastTransition = time.Now()
	if err := rm.store.SetAgent(agentState); err != nil {
		return fmt.Errorf("updating restart count for agent %s: %w", agentID, err)
	}

	// Start the agent with the manifest from the config.
	if err := rm.vmMgr.StartAgent(cfg.Manifest); err != nil {
		return fmt.Errorf("restarting agent %s: %w", agentID, err)
	}

	rm.logger.Info("agent restarted successfully",
		"agent_id", agentID,
		"restart_count", agentState.RestartCount,
	)

	return nil
}
