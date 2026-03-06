package health

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/nats-io/nats.go"
)

// Monitor watches for agent heartbeats on NATS and updates health state.
type Monitor struct {
	store          *state.Store
	nc             *nats.Conn
	logger         *slog.Logger
	interval       time.Duration
	maxFailures    int
	restartManager *RestartManager

	mu          sync.Mutex
	lastSeen    map[string]time.Time // agentID → last heartbeat time
	inRestart   map[string]struct{}  // agentIDs currently being restarted
	sub         *nats.Subscription
	stopCh      chan struct{}
	stopOnce    sync.Once // prevent double-close panic
	started     bool      // prevent double-start
}

// NewMonitor creates a new health monitor.
func NewMonitor(store *state.Store, nc *nats.Conn, interval time.Duration, maxFailures int, logger *slog.Logger) *Monitor {
	if interval == 0 {
		interval = 30 * time.Second
	}
	if maxFailures == 0 {
		maxFailures = 3
	}
	return &Monitor{
		store:       store,
		nc:          nc,
		logger:      logger.With("component", "health-monitor"),
		interval:    interval,
		maxFailures: maxFailures,
		lastSeen:    make(map[string]time.Time),
		inRestart:   make(map[string]struct{}),
		stopCh:      make(chan struct{}),
	}
}

// SetRestartManager configures the restart manager invoked when an agent becomes unhealthy.
// The monitor delegates to the RestartManager for policy-based restarts.
func (m *Monitor) SetRestartManager(rm *RestartManager) {
	m.restartManager = rm
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

	sub, err := m.nc.Subscribe("hive.health.*", m.handleHeartbeat)
	if err != nil {
		return fmt.Errorf("subscribing to health subjects: %w", err)
	}
	m.sub = sub

	go m.checkLoop()

	m.logger.Info("health monitor started", "interval", m.interval, "max_failures", m.maxFailures)
	return nil
}

// Stop stops the health monitor. Safe to call multiple times.
func (m *Monitor) Stop() {
	m.stopOnce.Do(func() {
		close(m.stopCh)
		if m.sub != nil {
			m.sub.Unsubscribe()
		}
	})
}

// RecordHeartbeat records a heartbeat for an agent (used when heartbeat arrives outside NATS).
func (m *Monitor) RecordHeartbeat(agentID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastSeen[agentID] = time.Now()
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

	m.mu.Lock()
	m.lastSeen[agentID] = time.Now()
	m.mu.Unlock()

	m.logger.Debug("heartbeat received", "agent_id", agentID)
}

func (m *Monitor) checkLoop() {
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

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, agent := range agents {
		if agent.Status != state.AgentStatusRunning {
			continue
		}

		lastSeen, ok := m.lastSeen[agent.ID]
		if !ok {
			// No heartbeat ever received, check if agent has been running long enough
			if now.Sub(agent.StartedAt) > threshold {
				m.logger.Warn("agent has never sent heartbeat",
					"agent_id", agent.ID,
					"started_at", agent.StartedAt,
				)
				m.markUnhealthy(agent.ID)
			}
			continue
		}

		if now.Sub(lastSeen) > threshold {
			m.logger.Warn("agent heartbeat timeout",
				"agent_id", agent.ID,
				"last_seen", lastSeen,
				"threshold", threshold,
			)
			m.markUnhealthy(agent.ID)
		}
	}
}

// markUnhealthy is called with m.mu held. It updates the agent status to
// FAILED in the state store and, if a RestartManager is configured, launches
// the restart in a background goroutine so that the mutex is released
// immediately and heartbeat processing is never blocked by backoff sleeps.
//
// The inRestart set prevents launching multiple concurrent restarts for the
// same agent; the goroutine removes the entry when it finishes.
func (m *Monitor) markUnhealthy(agentID string) {
	// Update state: transition to FAILED and record the error reason.
	agent := m.store.GetAgent(agentID)
	if agent == nil {
		return
	}

	agent.Status = state.AgentStatusFailed
	agent.Error = "heartbeat timeout: agent marked unhealthy"
	agent.LastTransition = time.Now()
	if err := m.store.SetAgent(agent); err != nil {
		m.logger.Error("failed to update agent state", "agent_id", agentID, "error", err)
	}

	// Invoke the restart manager asynchronously so the caller's mutex
	// is released immediately (backoff in HandleUnhealthy can be long).
	if m.restartManager == nil {
		return
	}

	// Guard against duplicate concurrent restarts for the same agent.
	if _, alreadyRestarting := m.inRestart[agentID]; alreadyRestarting {
		m.logger.Debug("restart already in progress, skipping", "agent_id", agentID)
		return
	}
	m.inRestart[agentID] = struct{}{}

	go func() {
		defer func() {
			m.mu.Lock()
			delete(m.inRestart, agentID)
			m.mu.Unlock()
		}()

		if err := m.restartManager.HandleUnhealthy(agentID); err != nil {
			m.logger.Error("restart manager error", "agent_id", agentID, "error", err)
		}
	}()
}

// RemoveAgent cleans up monitor state for a destroyed agent. Call this
// whenever an agent is removed so that the lastSeen map does not grow
// without bound over the lifetime of hived.
func (m *Monitor) RemoveAgent(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.lastSeen, id)
	delete(m.inRestart, id)
}
