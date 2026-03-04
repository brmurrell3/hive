package sidecar

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// Sidecar manages an agent's runtime, health, and NATS communication.
// It is the core component that runs alongside (or wraps) an agent process,
// providing health reporting, capability exposure, and control plane connectivity.
type Sidecar struct {
	agentID      string
	teamID       string
	config       Config
	natsConn     *nats.Conn
	runtime      *Runtime
	httpServer   *http.Server
	logger       *slog.Logger
	startTime    time.Time
	capabilities []Capability
	mu           sync.RWMutex
	healthy      bool
	stopCh       chan struct{}
	broadcastSub *nats.Subscription
}

// Config holds all configuration for a Sidecar instance.
type Config struct {
	// AgentID is the unique identifier for the agent this sidecar manages.
	AgentID string

	// TeamID is the team this agent belongs to.
	TeamID string

	// NATSUrl is the NATS server URL to connect to (e.g., "nats://localhost:4222").
	NATSUrl string

	// HTTPAddr is the listen address for the HTTP API. Defaults to ":9100".
	HTTPAddr string

	// Capabilities is the list of capabilities this agent exposes.
	Capabilities []Capability

	// RuntimeCmd is the command to start the agent runtime process.
	// If empty, no runtime process is started (no-op mode).
	RuntimeCmd string

	// RuntimeArgs are the arguments passed to the runtime command.
	RuntimeArgs []string

	// WorkspacePath is the working directory for the runtime process.
	WorkspacePath string

	// HealthInterval is the interval between heartbeat publications.
	// Defaults to 30 seconds if zero.
	HealthInterval time.Duration
}

// Capability describes a single capability that an agent exposes to the cluster.
type Capability struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Inputs      []CapabilityParam `json:"inputs,omitempty"`
	Outputs     []CapabilityParam `json:"outputs,omitempty"`
	Async       bool             `json:"async,omitempty"`
}

// CapabilityParam describes an input or output parameter for a capability.
type CapabilityParam struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required"`
}

// New creates a new Sidecar with the given configuration and logger.
// The sidecar is not started; call Start to begin operations.
func New(cfg Config, logger *slog.Logger) *Sidecar {
	if logger == nil {
		logger = slog.Default()
	}

	return &Sidecar{
		agentID:      cfg.AgentID,
		teamID:       cfg.TeamID,
		config:       cfg,
		logger:       logger.With("component", "sidecar", "agent_id", cfg.AgentID),
		capabilities: cfg.Capabilities,
		stopCh:       make(chan struct{}),
	}
}

// Start initializes and starts all sidecar subsystems: NATS connection,
// HTTP server, agent runtime, and heartbeat publishing. It blocks until all
// subsystems are started or an error occurs.
func (s *Sidecar) Start(ctx context.Context) error {
	s.startTime = time.Now()
	s.logger.Info("starting sidecar",
		"agent_id", s.agentID,
		"team_id", s.teamID,
	)

	// 1. Connect to NATS.
	if err := s.connectNATS(); err != nil {
		return fmt.Errorf("connecting to NATS: %w", err)
	}

	// 2. Subscribe to control messages.
	if err := s.subscribeControl(); err != nil {
		return fmt.Errorf("subscribing to control: %w", err)
	}

	// 3. Create and start the agent runtime.
	s.runtime = NewRuntime(
		s.config.RuntimeCmd,
		s.config.RuntimeArgs,
		s.config.WorkspacePath,
		s.logger,
	)
	if err := s.runtime.Start(); err != nil {
		s.closeNATS()
		return fmt.Errorf("starting runtime: %w", err)
	}

	// 4. Mark as healthy now that the runtime is up.
	s.mu.Lock()
	s.healthy = true
	s.mu.Unlock()

	// 5. Start the HTTP server for health and capabilities.
	if err := s.startHTTPServer(); err != nil {
		s.runtime.Stop()
		s.closeNATS()
		return fmt.Errorf("starting HTTP server: %w", err)
	}

	// 6. Start publishing heartbeats.
	s.startHeartbeat()

	// 7. Monitor the runtime; if it exits unexpectedly, mark unhealthy.
	go s.monitorRuntime()

	s.logger.Info("sidecar started successfully")
	return nil
}

// monitorRuntime watches the agent runtime process. If the runtime exits,
// the sidecar is marked unhealthy.
func (s *Sidecar) monitorRuntime() {
	// If there is no runtime command, there is nothing to monitor.
	if s.config.RuntimeCmd == "" {
		return
	}

	err := s.runtime.Wait()

	s.mu.Lock()
	s.healthy = false
	s.mu.Unlock()

	if err != nil {
		s.logger.Error("runtime exited unexpectedly, marking unhealthy",
			"error", err,
		)
	} else {
		s.logger.Warn("runtime exited, marking unhealthy")
	}
}

// Stop performs a graceful shutdown of all sidecar subsystems in reverse
// start order: heartbeat, HTTP server, runtime, NATS.
func (s *Sidecar) Stop() error {
	s.logger.Info("stopping sidecar")

	// Signal the heartbeat goroutine to stop.
	close(s.stopCh)

	// Shut down the HTTP server with a timeout.
	if s.httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.httpServer.Shutdown(ctx); err != nil {
			s.logger.Error("error shutting down HTTP server", "error", err)
		}
	}

	// Stop the agent runtime.
	if s.runtime != nil {
		if err := s.runtime.Stop(); err != nil {
			s.logger.Error("error stopping runtime", "error", err)
		}
	}

	// Close the NATS connection.
	s.closeNATS()

	s.mu.Lock()
	s.healthy = false
	s.mu.Unlock()

	s.logger.Info("sidecar stopped")
	return nil
}

// IsHealthy returns whether the sidecar and its runtime are both healthy.
func (s *Sidecar) IsHealthy() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.healthy && s.runtime.IsRunning()
}
