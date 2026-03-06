package sidecar

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/brmurrell3/hive/internal/capability"
	"github.com/brmurrell3/hive/internal/types"
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
	vsockProxy   *VsockProxy
	logger       *slog.Logger
	startTime    time.Time
	capabilities []Capability
	capRouter    *capability.Router
	mu           sync.RWMutex
	healthy      bool
	stopCh       chan struct{}
	stopOnce     sync.Once
	controlSub   *nats.Subscription
	memorySub    *nats.Subscription
	broadcastSub *nats.Subscription
}

// SidecarMode indicates how the sidecar is running.
type SidecarMode string

const (
	// ModeStandalone indicates the sidecar runs as a standalone binary (e.g., inside a VM).
	ModeStandalone SidecarMode = "standalone"

	// ModeLibrary indicates the sidecar runs as goroutines within a host process.
	ModeLibrary SidecarMode = "library"
)

// Config holds all configuration for a Sidecar instance.
type Config struct {
	// AgentID is the unique identifier for the agent this sidecar manages.
	AgentID string

	// TeamID is the team this agent belongs to.
	TeamID string

	// NATSUrl is the NATS server URL to connect to (e.g., "nats://localhost:4222").
	NATSUrl string

	// NATSToken is the auth token for the NATS server. If non-empty, the
	// sidecar will use it when connecting to NATS.
	NATSToken string

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

	// Tier is the tier of the agent (vm, native, firmware). Used in heartbeats.
	// Defaults to "vm" if empty.
	Tier string

	// Mode is the sidecar operating mode (standalone or library).
	// Defaults to standalone if empty.
	Mode SidecarMode

	// VsockEnabled enables the vsock-to-TCP proxy for Firecracker VMs.
	// When true, the sidecar starts a local TCP listener that proxies
	// connections to the host via virtio-vsock, allowing the NATS client
	// to connect through vsock without modification.
	VsockEnabled bool

	// VsockHostCID is the vsock CID of the host. Defaults to 2
	// (VMADDR_CID_HOST) which is the standard host CID for Firecracker.
	VsockHostCID uint32

	// VsockHostPort is the vsock port on the host where the NATS server
	// is reachable via the VsockForwarder. Defaults to 4222.
	VsockHostPort uint32
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

	// 1a. If vsock is enabled (running inside a Firecracker VM), start the
	// vsock-to-TCP proxy so the NATS client can connect to 127.0.0.1:<port>
	// and have the traffic forwarded to the host via virtio-vsock.
	if s.config.VsockEnabled {
		hostCID := s.config.VsockHostCID
		if hostCID == 0 {
			hostCID = 2 // VMADDR_CID_HOST
		}
		hostPort := s.config.VsockHostPort
		if hostPort == 0 {
			hostPort = 4222
		}
		// The proxy listens on the same address the NATS client will connect to.
		proxyAddr := fmt.Sprintf("127.0.0.1:%d", hostPort)
		s.vsockProxy = NewVsockProxy(proxyAddr, hostCID, hostPort, s.logger)
		if err := s.vsockProxy.Start(ctx); err != nil {
			return fmt.Errorf("starting vsock proxy: %w", err)
		}
		s.logger.Info("vsock proxy started, NATS traffic will be forwarded via vsock",
			"proxy_addr", proxyAddr,
			"host_cid", hostCID,
			"host_port", hostPort,
		)
	}

	// 1b. Connect to NATS.
	if err := s.connectNATS(); err != nil {
		if s.vsockProxy != nil {
			s.vsockProxy.Stop()
		}
		return fmt.Errorf("connecting to NATS: %w", err)
	}

	// 2. Subscribe to control messages.
	if err := s.subscribeControl(); err != nil {
		return fmt.Errorf("subscribing to control: %w", err)
	}

	// 2b. Subscribe to MEMORY.md updates pushed from hived.
	if err := s.subscribeMemoryUpdates(); err != nil {
		return fmt.Errorf("subscribing to memory updates: %w", err)
	}

	// 3. Create the capability router and register a handler for each declared
	// capability. Each handler is the local implementation that processes
	// incoming NATS capability requests. The router subscribes to
	// hive.capabilities.{agentID}.{cap}.request and dispatches to the handler.
	//
	// IMPORTANT: The handler must NOT call capRouter.Invoke() for its own agent,
	// as that would publish back to the same NATS subject the router subscribes
	// to, creating an infinite loop. Instead, the handler executes the capability
	// locally by forwarding to the runtime process via HTTP (if configured) or
	// returning an acknowledgment for no-op runtimes.
	s.capRouter = capability.NewRouter(s.agentID, s.natsConn, s.logger)
	for _, cap := range s.capabilities {
		capName := cap.Name
		s.capRouter.RegisterHandler(capName, func(inputs map[string]interface{}) (map[string]interface{}, error) {
			return s.executeCapabilityLocally(capName, inputs)
		})
	}
	if err := s.capRouter.Start(); err != nil {
		s.closeNATS()
		return fmt.Errorf("starting capability router: %w", err)
	}

	// 5. Create and start the agent runtime.
	s.runtime = NewRuntime(
		s.config.RuntimeCmd,
		s.config.RuntimeArgs,
		s.config.WorkspacePath,
		s.logger,
	)
	if err := s.runtime.Start(); err != nil {
		s.capRouter.Stop()
		s.closeNATS()
		return fmt.Errorf("starting runtime: %w", err)
	}

	// 6. Mark as healthy now that the runtime is up.
	s.mu.Lock()
	s.healthy = true
	s.mu.Unlock()

	// 7. Start the HTTP server for health and capabilities.
	if err := s.startHTTPServer(); err != nil {
		s.runtime.Stop()
		s.capRouter.Stop()
		s.closeNATS()
		return fmt.Errorf("starting HTTP server: %w", err)
	}

	// 8. Start publishing heartbeats.
	s.startHeartbeat()

	// 9. Monitor the runtime; if it exits unexpectedly, mark unhealthy.
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
// Safe to call multiple times.
func (s *Sidecar) Stop() error {
	s.logger.Info("stopping sidecar")

	// Use sync.Once to prevent double-close panic.
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})

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

	// Unsubscribe from control and memory NATS subjects.
	if s.controlSub != nil {
		if err := s.controlSub.Unsubscribe(); err != nil {
			s.logger.Warn("error unsubscribing from control subject", "error", err)
		}
	}
	if s.memorySub != nil {
		if err := s.memorySub.Unsubscribe(); err != nil {
			s.logger.Warn("error unsubscribing from memory subject", "error", err)
		}
	}

	// Stop the capability router (unsubscribes from NATS subjects).
	if s.capRouter != nil {
		s.capRouter.Stop()
	}

	// Close the NATS connection.
	s.closeNATS()

	// Stop the vsock proxy if it was started.
	if s.vsockProxy != nil {
		s.vsockProxy.Stop()
	}

	s.mu.Lock()
	s.healthy = false
	s.mu.Unlock()

	s.logger.Info("sidecar stopped")
	return nil
}

// capabilityRequestTimeout is how long executeCapabilityLocally waits for the
// runtime process to produce a response file before returning an async ack.
const capabilityRequestTimeout = 10 * time.Second

// capabilityPollInterval is how frequently we check for a response file.
const capabilityPollInterval = 50 * time.Millisecond

// capabilityRequest is the JSON structure written to the requests directory
// for the runtime process to pick up and execute.
type capabilityRequest struct {
	ID         string                 `json:"id"`
	Capability string                 `json:"capability"`
	AgentID    string                 `json:"agent_id"`
	Inputs     map[string]interface{} `json:"inputs"`
	Timestamp  string                 `json:"timestamp"`
}

// executeCapabilityLocally processes a capability request without going through
// NATS. This avoids the infinite loop that would occur if the handler published
// back to the same NATS subject the router subscribes to.
//
// If no runtime command is configured (no-op mode), the handler returns an
// acknowledgment with the inputs echoed back -- there is no process to forward
// to.
//
// If a runtime IS configured and running, the sidecar writes the request as a
// JSON file to {workspace}/.hive/requests/{uuid}.json and waits (with timeout)
// for the runtime to produce a response at {uuid}.response.json. This
// file-based protocol lets any runtime process (Python, Node, shell script,
// etc.) handle capability requests without requiring a specific IPC mechanism.
func (s *Sidecar) executeCapabilityLocally(capName string, inputs map[string]interface{}) (map[string]interface{}, error) {
	s.logger.Info("executing capability locally",
		"capability", capName,
		"agent_id", s.agentID,
	)

	// Check if the runtime is running; if not, the capability cannot be executed.
	if s.runtime != nil && !s.runtime.IsRunning() {
		return nil, fmt.Errorf("runtime is not running, cannot execute capability %s", capName)
	}

	// No-op mode: no runtime command configured. Echo the inputs back as an
	// acknowledgment. This is the correct behavior for agents that don't have
	// a runtime process (e.g., pure capability placeholders or test agents).
	if s.config.RuntimeCmd == "" {
		result := map[string]interface{}{
			"capability": capName,
			"agent_id":   s.agentID,
			"status":     "executed",
		}
		if inputs != nil {
			result["inputs_received"] = inputs
		}
		return result, nil
	}

	// Runtime is configured -- forward the request via the filesystem.
	return s.forwardToRuntime(capName, inputs)
}

// forwardToRuntime writes a capability request to the workspace requests
// directory and waits for the runtime process to produce a response file.
func (s *Sidecar) forwardToRuntime(capName string, inputs map[string]interface{}) (map[string]interface{}, error) {
	workspace := s.config.WorkspacePath
	if workspace == "" {
		workspace = "."
	}

	requestsDir := filepath.Join(workspace, ".hive", "requests")
	if err := os.MkdirAll(requestsDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating requests directory: %w", err)
	}

	reqID := types.NewUUID()
	reqPath := filepath.Join(requestsDir, reqID+".json")
	respPath := filepath.Join(requestsDir, reqID+".response.json")

	// Build and marshal the request.
	req := capabilityRequest{
		ID:         reqID,
		Capability: capName,
		AgentID:    s.agentID,
		Inputs:     inputs,
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshalling capability request: %w", err)
	}

	// Write the request file atomically so the runtime never sees a partial file.
	if err := writeFileAtomic(reqPath, data); err != nil {
		return nil, fmt.Errorf("writing capability request file: %w", err)
	}

	s.logger.Info("capability request written, waiting for response",
		"capability", capName,
		"request_id", reqID,
		"request_path", reqPath,
	)

	// Poll for the response file with a timeout.
	deadline := time.After(capabilityRequestTimeout)
	ticker := time.NewTicker(capabilityPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			// Timeout -- the runtime has not responded yet. Return an async
			// acknowledgment so the caller knows the request was submitted.
			s.logger.Warn("capability request timed out waiting for response",
				"capability", capName,
				"request_id", reqID,
			)
			return map[string]interface{}{
				"capability": capName,
				"agent_id":   s.agentID,
				"request_id": reqID,
				"status":     "submitted",
				"message":    "request submitted to runtime, response pending",
			}, nil

		case <-ticker.C:
			respData, err := os.ReadFile(respPath)
			if err != nil {
				if os.IsNotExist(err) {
					continue // Not ready yet, keep polling.
				}
				return nil, fmt.Errorf("reading capability response file: %w", err)
			}

			// Response file exists -- parse it and return.
			var result map[string]interface{}
			if err := json.Unmarshal(respData, &result); err != nil {
				return nil, fmt.Errorf("unmarshalling capability response: %w", err)
			}

			s.logger.Info("capability response received from runtime",
				"capability", capName,
				"request_id", reqID,
			)

			// Clean up the request and response files. Best-effort; errors
			// are logged but do not fail the operation.
			if rmErr := os.Remove(reqPath); rmErr != nil {
				s.logger.Warn("failed to clean up request file", "path", reqPath, "error", rmErr)
			}
			if rmErr := os.Remove(respPath); rmErr != nil {
				s.logger.Warn("failed to clean up response file", "path", respPath, "error", rmErr)
			}

			return result, nil
		}
	}
}

// IsHealthy returns whether the sidecar and its runtime are both healthy.
func (s *Sidecar) IsHealthy() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.healthy && s.runtime != nil && s.runtime.IsRunning()
}
