// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

// Package health monitors agent heartbeats via NATS and triggers automatic restarts with exponential backoff.
package health

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/brmurrell3/hive/internal/protocol"
	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/nats-io/nats.go"
)

const (
	// maxConcurrentRestarts is the maximum number of restart goroutines allowed
	// to run simultaneously. When the limit is reached, new restarts are skipped
	// and will be retried on the next health check cycle.
	maxConcurrentRestarts = 10

	// defaultHealthCheckInterval is the default interval between heartbeat staleness checks.
	defaultHealthCheckInterval = 30 * time.Second

	// restartWaitTimeout is how long Stop() waits for in-flight restart goroutines
	// before giving up.
	restartWaitTimeout = 30 * time.Second

	// staleEntryCleanupInterval is how often the monitor cleans up lastSeen/inRestart
	// entries for agents that no longer exist in the state store.
	staleEntryCleanupInterval = 5 * time.Minute
)

// Monitor watches for agent heartbeats on NATS and updates health state.
// Monitor is single-use. Create a new Monitor for each lifecycle.
type Monitor struct {
	store          *state.Store
	nc             *nats.Conn
	logger         *slog.Logger
	interval       time.Duration
	maxFailures    int
	restartManager *RestartManager

	mu             sync.Mutex
	lastSeen       map[string]time.Time          // agentID → last heartbeat time
	inRestart      map[string]struct{}           // agentIDs currently being restarted
	restartCancels map[string]context.CancelFunc // per-agent restart cancel functions
	restartSem     chan struct{}                 // semaphore limiting concurrent restart goroutines
	sub            *nats.Subscription
	stopCh         chan struct{}
	stopOnce       sync.Once          // prevent double-close panic
	started        bool               // prevent double-start
	wg             sync.WaitGroup     // tracks checkLoop and cleanupLoop goroutines
	restartWg      sync.WaitGroup     // tracks restart goroutines
	cancelCtx      context.Context    // context cancelled on Stop to interrupt restart goroutines
	cancelFn       context.CancelFunc // cancel function for cancelCtx
}

// NewMonitor creates a new health monitor.
func NewMonitor(store *state.Store, nc *nats.Conn, interval time.Duration, maxFailures int, logger *slog.Logger) *Monitor {
	if interval == 0 {
		interval = defaultHealthCheckInterval
	}
	if maxFailures == 0 {
		maxFailures = 3
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Monitor{
		store:          store,
		nc:             nc,
		logger:         logger.With("component", "health-monitor"),
		interval:       interval,
		maxFailures:    maxFailures,
		lastSeen:       make(map[string]time.Time),
		inRestart:      make(map[string]struct{}),
		restartCancels: make(map[string]context.CancelFunc),
		restartSem:     make(chan struct{}, maxConcurrentRestarts),
		stopCh:         make(chan struct{}),
		cancelCtx:      ctx,
		cancelFn:       cancel,
	}
}

// SetRestartManager configures the restart manager invoked when an agent becomes unhealthy.
// The monitor delegates to the RestartManager for policy-based restarts.
// It also registers a callback so that successful restarts reset the agent's
// heartbeat timer, giving it a grace period to send its first heartbeat.
func (m *Monitor) SetRestartManager(rm *RestartManager) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.restartManager = rm
	if rm != nil {
		rm.SetOnRestart(m.ResetHeartbeat)
	}
}

// Start begins monitoring heartbeats.
// Returns error if already started.
func (m *Monitor) Start() error {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return fmt.Errorf("health monitor already started")
	}
	m.started = true
	m.mu.Unlock()

	sub, err := m.nc.Subscribe(protocol.SubjHealthAll, m.handleHeartbeat)
	if err != nil {
		return fmt.Errorf("subscribing to health subjects: %w", err)
	}
	m.mu.Lock()
	m.sub = sub
	m.mu.Unlock()

	m.wg.Add(2)
	go m.checkLoop()
	go m.cleanupLoop()

	m.logger.Info("health monitor started", "interval", m.interval, "max_failures", m.maxFailures)
	return nil
}

// Stop stops the health monitor. Safe to call multiple times.
// It cancels the context to interrupt any pending restart backoff sleeps.
func (m *Monitor) Stop() {
	m.stopOnce.Do(func() {
		close(m.stopCh)
		m.cancelFn() // cancel context to interrupt restart goroutine backoff sleeps
		m.mu.Lock()
		sub := m.sub
		m.mu.Unlock()
		if sub != nil {
			sub.Unsubscribe()
		}
		m.wg.Wait() // wait for checkLoop and cleanupLoop to exit

		// Wait for all restart goroutines to finish, with a timeout to
		// prevent hanging indefinitely if a VMManager call is stuck.
		restartDone := make(chan struct{})
		go func() {
			m.restartWg.Wait()
			close(restartDone)
		}()
		select {
		case <-restartDone:
		case <-time.After(restartWaitTimeout):
			m.logger.Warn("health monitor: timed out waiting for restart goroutines")
		}
	})
}

// RecordHeartbeat records a heartbeat for an agent (used when heartbeat arrives outside NATS).
// It also clears the inRestart flag, since a heartbeat proves the agent is alive and
// no longer needs restart protection.
func (m *Monitor) RecordHeartbeat(agentID string) {
	if agentID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastSeen[agentID] = time.Now()
	delete(m.inRestart, agentID)
}

// ResetHeartbeat sets the lastSeen time for an agent to now. This is used
// after a successful restart to give the agent a fresh grace period to send
// its first heartbeat before the monitor marks it unhealthy again.
// It also clears the inRestart flag since the restart completed and the agent
// has been given a grace period.
func (m *Monitor) ResetHeartbeat(agentID string) {
	if agentID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastSeen[agentID] = time.Now()
	delete(m.inRestart, agentID)
}

// LastSeen returns the last heartbeat time for an agent.
func (m *Monitor) LastSeen(agentID string) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.lastSeen[agentID]
	return t, ok
}

func (m *Monitor) handleHeartbeat(msg *nats.Msg) {
	var env types.Envelope
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		m.logger.Warn("invalid heartbeat envelope", "error", err)
		return
	}

	if env.Type != types.MessageTypeHealth {
		return
	}

	agentID := env.From
	if agentID == "" {
		m.logger.Warn("heartbeat missing agent ID")
		return
	}

	if err := types.ValidateSubjectComponent("agent_id", agentID); err != nil {
		return
	}

	// Cross-validate: the agent ID in the envelope must match the agent ID
	// encoded in the NATS subject (hive.health.<agentID>). This prevents a
	// compromised agent from spoofing heartbeats for a different agent.
	parts := strings.Split(msg.Subject, ".")
	if len(parts) == 3 {
		subjectAgentID := parts[2]
		if subjectAgentID != agentID {
			m.logger.Warn("heartbeat subject/from mismatch, dropping",
				"subject_agent_id", subjectAgentID,
				"envelope_from", agentID,
			)
			return
		}
	}

	// Unmarshal the health payload to check the Healthy field. If the agent
	// explicitly reports itself as unhealthy, do not update lastSeen so the
	// health check loop will eventually trigger markUnhealthy.
	var hp types.HealthPayload
	if err := json.Unmarshal(env.Payload, &hp); err != nil {
		m.logger.Warn("failed to unmarshal health payload", "agent_id", agentID, "error", err)
		return
	}

	if !hp.Healthy {
		m.logger.Warn("agent reported unhealthy via heartbeat", "agent_id", agentID)
		return
	}

	m.mu.Lock()
	m.lastSeen[agentID] = time.Now()
	delete(m.inRestart, agentID) // agent is alive, clear restart guard
	m.mu.Unlock()

	// If an agent is in FAILED state but sending healthy heartbeats, it has
	// recovered (common for external agents whose runtime takes time to start).
	// Transition back to RUNNING by removing the stale entry and recreating it.
	if agent := m.store.GetAgent(agentID); agent != nil && agent.Status == state.AgentStatusFailed {
		team := agent.Team
		nodeID := agent.NodeID
		if err := m.store.RemoveAgent(agentID); err == nil {
			now := time.Now().UTC()
			if err := m.store.SetAgent(&state.AgentState{
				ID:             agentID,
				Team:           team,
				Status:         state.AgentStatusRunning,
				NodeID:         nodeID,
				LastTransition: now,
				StartedAt:      now,
			}); err != nil {
				m.logger.Warn("failed to recover agent state", "agent_id", agentID, "error", err)
			} else {
				m.logger.Info("agent recovered from FAILED state via healthy heartbeat", "agent_id", agentID)
			}
		}
	}

	m.logger.Debug("heartbeat received", "agent_id", agentID)
}

func (m *Monitor) checkLoop() {
	defer m.wg.Done()
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.checkAgents()
		}
	}
}

func (m *Monitor) checkAgents() {
	agents := m.store.AllAgents()
	now := time.Now()
	threshold := m.interval * time.Duration(m.maxFailures)

	// Collect unhealthy agents under the lock, then release before calling
	// markUnhealthy (which calls store.ModifyAgent and may block).
	type unhealthyAgent struct {
		id     string
		reason string
	}

	m.mu.Lock()
	var unhealthyAgents []unhealthyAgent
	for _, agent := range agents {
		if agent.Status != state.AgentStatusRunning {
			continue
		}

		lastSeen, ok := m.lastSeen[agent.ID]
		if !ok {
			// No heartbeat ever received, check if agent has been running long enough.
			// agent.StartedAt comes from the state store (serialized) so it lacks a
			// monotonic clock reading. If the wall clock moved backward, Sub can
			// return a negative duration — treat that as exceeding the threshold.
			sinceStart := now.Sub(agent.StartedAt)
			if sinceStart < 0 {
				sinceStart = threshold + 1
			}
			if sinceStart > threshold {
				m.logger.Warn("agent has never sent heartbeat",
					"agent_id", agent.ID,
					"started_at", agent.StartedAt,
				)
				unhealthyAgents = append(unhealthyAgents, unhealthyAgent{
					id:     agent.ID,
					reason: "never sent heartbeat",
				})
			}
			continue
		}

		// lastSeen is always set via time.Now() so it carries a monotonic reading;
		// now.Sub(lastSeen) will use the monotonic clock and is normally immune to
		// wall-clock adjustments. However, if the process was restored from a
		// checkpoint or the runtime is buggy, guard against negative durations.
		elapsed := now.Sub(lastSeen)
		if elapsed < 0 {
			// Clock moved backward — treat as if we haven't heard from the agent
			// recently so the unhealthy check triggers.
			elapsed = threshold + 1
		}
		if elapsed > threshold {
			m.logger.Warn("agent heartbeat timeout",
				"agent_id", agent.ID,
				"last_seen", lastSeen,
				"threshold", threshold,
			)
			unhealthyAgents = append(unhealthyAgents, unhealthyAgent{
				id:     agent.ID,
				reason: "heartbeat timeout",
			})
		}
	}
	m.mu.Unlock()

	// Now process outside the lock.
	for _, ua := range unhealthyAgents {
		m.markUnhealthy(ua.id)
	}
}

// markUnhealthy updates the agent status to FAILED in the state store and,
// if a RestartManager is configured, launches the restart in a background
// goroutine so that heartbeat processing is never blocked by backoff sleeps.
//
// The inRestart set prevents launching multiple concurrent restarts for the
// same agent. On successful restart, the flag remains set until the agent
// sends a heartbeat (cleared in RecordHeartbeat/ResetHeartbeat) or is removed.
// On failed restart, the flag is cleared so the next check cycle can retry.
func (m *Monitor) markUnhealthy(agentID string) {
	// Update state atomically: transition to FAILED and record the error reason.
	// ModifyAgent enforces valid state transitions internally.
	if err := m.store.ModifyAgent(agentID, func(a *state.AgentState) error {
		a.Status = state.AgentStatusFailed
		a.Error = "heartbeat timeout: agent marked unhealthy"
		a.LastTransition = time.Now()
		return nil
	}); err != nil {
		m.logger.Error("failed to update agent state", "agent_id", agentID, "error", err)
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Invoke the restart manager asynchronously so the mutex
	// is released immediately (backoff in HandleUnhealthyCtx can be long).
	rm := m.restartManager
	if rm == nil {
		return
	}

	// Guard against duplicate concurrent restarts for the same agent.
	if _, alreadyRestarting := m.inRestart[agentID]; alreadyRestarting {
		m.logger.Debug("restart already in progress, skipping", "agent_id", agentID)
		return
	}
	m.inRestart[agentID] = struct{}{}

	// Try to acquire the restart semaphore to limit concurrent goroutines.
	// If at capacity, skip this restart — the next health check will retry.
	select {
	case m.restartSem <- struct{}{}:
		// Acquired semaphore slot.
	default:
		delete(m.inRestart, agentID)
		m.logger.Warn("restart semaphore full, deferring restart to next check cycle",
			"agent_id", agentID,
			"max_concurrent", maxConcurrentRestarts,
		)
		return
	}

	// Create a per-agent context derived from the monitor-wide context so that
	// both RemoveAgent (per-agent cancel) and Stop (monitor-wide cancel) can
	// interrupt the restart goroutine's backoff sleep.
	agentCtx, agentCancel := context.WithCancel(m.cancelCtx)
	m.restartCancels[agentID] = agentCancel

	m.restartWg.Add(1)
	go func() {
		defer m.restartWg.Done()
		defer func() {
			agentCancel()  // ensure context resources are freed
			<-m.restartSem // release semaphore slot
			m.mu.Lock()
			// Do NOT delete inRestart[agentID] here. The flag must remain set
			// until the agent proves it is alive by sending a heartbeat (cleared
			// in RecordHeartbeat / ResetHeartbeat) or the agent is removed
			// (cleared in RemoveAgent / cleanupStaleEntries). Clearing it here
			// would allow checkAgents to launch a duplicate restart goroutine
			// before the newly-restarted agent has had time to heartbeat.
			delete(m.restartCancels, agentID)
			m.mu.Unlock()
		}()

		if err := rm.HandleUnhealthyCtx(agentCtx, agentID); err != nil {
			m.logger.Error("restart manager error", "agent_id", agentID, "error", err)
			// Restart failed — clear inRestart so the next health check cycle
			// can retry. Without this, a failed restart would permanently block
			// future restart attempts for this agent.
			m.mu.Lock()
			delete(m.inRestart, agentID)
			m.mu.Unlock()
		}
	}()
}

// cleanupLoop periodically removes entries from the lastSeen and inRestart
// maps for agents that no longer exist in the state store. This prevents
// unbounded map growth if RemoveAgent is not called for every destroyed agent.
func (m *Monitor) cleanupLoop() {
	defer m.wg.Done()
	ticker := time.NewTicker(staleEntryCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.cleanupStaleEntries()
		}
	}
}

// cleanupStaleEntries removes lastSeen and inRestart entries for agents
// that no longer exist in the state store.
func (m *Monitor) cleanupStaleEntries() {
	// Build a set of all known agent IDs from the store.
	agents := m.store.AllAgents()
	knownAgents := make(map[string]struct{}, len(agents))
	for _, a := range agents {
		knownAgents[a.ID] = struct{}{}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for id := range m.lastSeen {
		if _, exists := knownAgents[id]; !exists {
			delete(m.lastSeen, id)
			m.logger.Debug("cleaned up stale lastSeen entry", "agent_id", id)
		}
	}
	for id := range m.inRestart {
		if _, exists := knownAgents[id]; !exists {
			delete(m.inRestart, id)
			m.logger.Debug("cleaned up stale inRestart entry", "agent_id", id)
		}
	}
	for id, cancel := range m.restartCancels {
		if _, exists := knownAgents[id]; !exists {
			cancel()
			delete(m.restartCancels, id)
			m.logger.Debug("cleaned up stale restartCancel entry", "agent_id", id)
		}
	}
}

// RemoveAgent cleans up monitor state for a destroyed agent. Call this
// whenever an agent is removed so that the lastSeen map does not grow
// without bound over the lifetime of hived. If a restart is in progress
// for this agent, it is cancelled.
func (m *Monitor) RemoveAgent(id string) {
	m.mu.Lock()
	cancel := m.restartCancels[id]
	delete(m.lastSeen, id)
	delete(m.inRestart, id)
	delete(m.restartCancels, id)
	m.mu.Unlock()

	// Cancel outside the lock to avoid holding the mutex while the restart
	// goroutine's deferred cleanup tries to acquire it.
	if cancel != nil {
		cancel()
	}
}
