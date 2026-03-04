package health

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hivehq/hive/internal/state"
	"github.com/hivehq/hive/internal/types"
	"github.com/nats-io/nats.go"
)

// Monitor watches for agent heartbeats on NATS and updates health state.
type Monitor struct {
	store          *state.Store
	nc             *nats.Conn
	logger         *slog.Logger
	interval       time.Duration
	maxFailures    int
	restartManager *RestartManager // T2-01: integration with RestartManager

	mu          sync.Mutex
	lastSeen    map[string]time.Time // agentID → last heartbeat time
	sub         *nats.Subscription
	stopCh      chan struct{}
	stopOnce    sync.Once // T3-01: prevent double-close panic
	started     bool      // T3-02: prevent double-start
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
		stopCh:      make(chan struct{}),
	}
}

// SetRestartManager configures the restart manager invoked when an agent becomes unhealthy.
// T2-01: The monitor delegates to the RestartManager for policy-based restarts.
func (m *Monitor) SetRestartManager(rm *RestartManager) {
	m.restartManager = rm
}

// Start begins monitoring heartbeats.
// T3-02: Returns error if already started.
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

// Stop stops the health monitor. Safe to call multiple times (T3-01).
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

func (m *Monitor) markUnhealthy(agentID string) {
	agent := m.store.GetAgent(agentID)
	if agent == nil {
		return
	}

	agent.Error = "heartbeat timeout: agent marked unhealthy"
	if err := m.store.SetAgent(agent); err != nil {
		m.logger.Error("failed to update agent state", "agent_id", agentID, "error", err)
	}

	// T2-01: Invoke the restart manager if configured.
	if m.restartManager != nil {
		if err := m.restartManager.HandleUnhealthy(agentID); err != nil {
			m.logger.Error("restart manager error", "agent_id", agentID, "error", err)
		}
	}
}
