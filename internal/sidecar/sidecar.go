// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

// Package sidecar implements the agent runtime, HTTP capability API, NATS heartbeats, and control message handling.
package sidecar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/brmurrell3/hive/internal/capability"
	"github.com/brmurrell3/hive/internal/protocol"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/nats-io/nats.go"
)

// Sidecar manages an agent's runtime, health, and NATS communication.
// It is the core component that runs alongside (or wraps) an agent process,
// providing health reporting, capability exposure, and control plane connectivity.
type Sidecar struct {
	agentID       string
	teamID        string
	config        Config
	natsConn      *nats.Conn
	runtime       *Runtime
	httpServer    *http.Server
	vsockProxy    *VsockProxy
	logger        *slog.Logger
	startTime     time.Time
	capabilities  []Capability
	capRouter     *capability.Router
	mu            sync.RWMutex
	healthy       bool
	stopCh        chan struct{}
	stopOnce      sync.Once
	wg            sync.WaitGroup // tracks background goroutines (heartbeat, monitorRuntime)
	controlSub    *nats.Subscription
	memorySub     *nats.Subscription
	broadcastSub  *nats.Subscription
	monitorCancel context.CancelFunc // cancels the current monitorRuntime goroutine
	envVars       []string           // extra env vars for the runtime process
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

	// CapabilityTimeout is how long to wait for the runtime process to produce
	// a response file before returning an async acknowledgment.
	// Defaults to 30 seconds if zero.
	CapabilityTimeout time.Duration

	// SecretNames lists the secret names this agent needs injected.
	SecretNames []string

	// TLSCertFile is the path to the TLS certificate for the HTTP server.
	// If both TLSCertFile and TLSKeyFile are set, the HTTP server uses HTTPS.
	TLSCertFile string

	// TLSKeyFile is the path to the TLS private key for the HTTP server.
	TLSKeyFile string

	// HTTPToken is a Bearer token required for all HTTP API endpoints except
	// /health. If empty, no authentication is enforced.
	HTTPToken string

	// CallbackURL is the base URL of the agent process's HTTP server for
	// receiving capability invocations. When set, the sidecar forwards
	// capability requests via HTTP POST to {CallbackURL}/handle/{capability}
	// instead of using file-based IPC. The agent process must run an HTTP
	// server at this URL.
	CallbackURL string

	// IsLead indicates this agent is the lead of its team. When true and
	// CallbackURL is set, the sidecar subscribes to team broadcasts and
	// forwards them to {CallbackURL}/handle/orchestrate so the lead agent
	// can coordinate the pipeline.
	IsLead bool
}

// Capability describes a single capability that an agent exposes to the cluster.
type Capability struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Inputs      []CapabilityParam `json:"inputs,omitempty"`
	Outputs     []CapabilityParam `json:"outputs,omitempty"`
	Async       bool              `json:"async,omitempty"`
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

// Start initializes and starts all sidecar subsystems: vsock proxy, NATS
// connection, capability router, agent runtime, HTTP server, and heartbeat
// publishing. It blocks until all subsystems are started or an error occurs.
// On failure, previously started subsystems are cleaned up in reverse order.
func (s *Sidecar) Start(ctx context.Context) error {
	// Validate agent and team IDs before using them in NATS subjects.
	if err := types.ValidateSubjectComponent("agent_id", s.agentID); err != nil {
		return fmt.Errorf("invalid sidecar configuration: %w", err)
	}
	if s.teamID != "" {
		if err := types.ValidateSubjectComponent("team_id", s.teamID); err != nil {
			return fmt.Errorf("invalid sidecar configuration: %w", err)
		}
	}

	s.startTime = time.Now()
	s.logger.Info("starting sidecar",
		"agent_id", s.agentID,
		"team_id", s.teamID,
	)

	// Phase 1: Vsock proxy (if running inside a Firecracker VM).
	if err := s.startVsock(ctx); err != nil {
		return fmt.Errorf("starting vsock proxy: %w", err)
	}

	// Phase 2: NATS connection and subscriptions.
	if err := s.startNATS(ctx); err != nil {
		s.cleanupVsock()
		return fmt.Errorf("starting NATS: %w", err)
	}

	// Phase 3: Capability router and control plane registration.
	if err := s.registerCapabilities(); err != nil {
		s.closeNATS()
		s.cleanupVsock()
		return fmt.Errorf("registering capabilities: %w", err)
	}

	// Phase 4: Agent runtime (secrets, process, callback readiness, metadata).
	if err := s.startRuntime(ctx); err != nil {
		s.cleanupCapRouter()
		s.closeNATS()
		s.cleanupVsock()
		return fmt.Errorf("starting runtime: %w", err)
	}

	// Phase 5: HTTP server for health and capability endpoints.
	if err := s.startHTTPServer(); err != nil {
		s.runtime.Stop() //nolint:errcheck // best-effort cleanup on HTTP server start failure
		s.cleanupCapRouter()
		s.closeNATS()
		s.cleanupVsock()
		return fmt.Errorf("starting HTTP server: %w", err)
	}

	// Post-startup: heartbeats, lead broadcast subscription, runtime monitor.
	s.startPostStartup()

	s.logger.Info("sidecar started successfully")
	return nil
}

// startVsock starts the vsock-to-TCP proxy if VsockEnabled is set. The proxy
// allows the NATS client inside a Firecracker VM to connect to 127.0.0.1:<port>
// and have the traffic forwarded to the host via virtio-vsock.
func (s *Sidecar) startVsock(ctx context.Context) error {
	if !s.config.VsockEnabled {
		return nil
	}

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
		return err
	}
	s.logger.Info("vsock proxy started, NATS traffic will be forwarded via vsock",
		"proxy_addr", proxyAddr,
		"host_cid", hostCID,
		"host_port", hostPort,
	)
	return nil
}

// cleanupVsock stops the vsock proxy if it was started. Used during cleanup
// on Start() failure.
func (s *Sidecar) cleanupVsock() {
	if s.vsockProxy != nil {
		s.vsockProxy.Stop()
	}
}

// startNATS connects to the NATS server and subscribes to control and memory
// update subjects. On failure the NATS connection is not cleaned up here; the
// caller is responsible for calling closeNATS().
func (s *Sidecar) startNATS(ctx context.Context) error {
	if err := s.connectNATS(ctx); err != nil {
		return fmt.Errorf("connecting to NATS: %w", err)
	}

	if err := s.subscribeControl(); err != nil {
		s.closeNATS()
		return fmt.Errorf("subscribing to control: %w", err)
	}

	if err := s.subscribeMemoryUpdates(); err != nil {
		s.closeNATS()
		return fmt.Errorf("subscribing to memory updates: %w", err)
	}

	return nil
}

// registerCapabilities creates the capability router and registers a handler
// for each declared capability. Each handler is the local implementation that
// processes incoming NATS capability requests. After starting the router, the
// capabilities are announced to the control plane for cross-team routing and
// discovery.
//
// IMPORTANT: The handler must NOT call capRouter.Invoke() for its own agent,
// as that would publish back to the same NATS subject the router subscribes
// to, creating an infinite loop. Instead, the handler executes the capability
// locally by forwarding to the runtime process via HTTP (if configured) or
// returning an acknowledgment for no-op runtimes.
func (s *Sidecar) registerCapabilities() error {
	s.mu.Lock()
	oldRouter := s.capRouter
	s.mu.Unlock()
	if oldRouter != nil {
		oldRouter.Stop()
	}

	router := capability.NewRouter(s.agentID, s.natsConn, s.logger)
	for _, cap := range s.capabilities {
		capName := cap.Name
		if err := router.RegisterHandler(capName, func(_ context.Context, inputs map[string]interface{}) (map[string]interface{}, error) {
			return s.executeCapabilityLocally(capName, inputs)
		}); err != nil {
			return fmt.Errorf("registering capability handler %q: %w", capName, err)
		}
	}
	if err := router.Start(); err != nil {
		return fmt.Errorf("starting capability router: %w", err)
	}
	s.mu.Lock()
	s.capRouter = router
	s.mu.Unlock()

	// Announce capabilities to the control plane so the capability registry
	// is populated for cross-team routing and discovery.
	if len(s.capabilities) > 0 {
		regPayload, _ := json.Marshal(map[string]interface{}{
			"agent_id":     s.agentID,
			"team_id":      s.config.TeamID,
			"tier":         s.config.Tier,
			"capabilities": s.capabilities,
		})
		if err := s.natsConn.Publish(protocol.SubjCapabilityRegister, regPayload); err != nil {
			s.logger.Warn("failed to publish capability registration", "error", err)
		} else {
			s.logger.Info("published capability registration to control plane",
				"count", len(s.capabilities))
		}
	}

	return nil
}

// cleanupCapRouter stops the capability router if it was started. Used during
// cleanup on Start() failure.
func (s *Sidecar) cleanupCapRouter() {
	s.mu.RLock()
	router := s.capRouter
	s.mu.RUnlock()
	if router != nil {
		router.Stop()
	}
}

// startRuntime fetches secrets, creates and starts the agent runtime process,
// waits for the agent's callback HTTP server to be ready (if configured),
// marks the sidecar as healthy, and writes workspace metadata.
func (s *Sidecar) startRuntime(_ context.Context) error {
	// Fetch and inject secrets if configured.
	// SECURITY: Secrets are passed via environment variables. On Linux,
	// /proc/<pid>/environ is readable by same-UID processes. Firecracker VM
	// isolation mitigates this for Tier 1 agents.
	var secretEnv []string
	if len(s.config.SecretNames) > 0 {
		fetchedSecrets, err := s.fetchSecrets(s.config.SecretNames)
		if err != nil {
			s.logger.Warn("failed to fetch secrets, continuing without", "error", err)
		} else {
			for name, value := range fetchedSecrets {
				secretEnv = append(secretEnv, fmt.Sprintf("%s=%s", name, value))
			}
			s.logger.Info("secrets fetched", "count", len(fetchedSecrets))
		}
	}

	// Create and start the agent runtime.
	s.runtime = NewRuntime(
		s.config.RuntimeCmd,
		s.config.RuntimeArgs,
		s.config.WorkspacePath,
		s.logger,
	)
	// Merge secret env vars with any env vars set via SetEnvVars.
	envs := make([]string, 0, len(s.envVars)+len(secretEnv))
	envs = append(envs, s.envVars...)
	envs = append(envs, secretEnv...)
	s.runtime.EnvVars = envs
	if err := s.runtime.Start(); err != nil {
		return fmt.Errorf("starting runtime: %w", err)
	}

	// Wait for the agent's callback HTTP server to be ready before accepting
	// capability requests. Without this, NATS capability subscriptions
	// (registered in registerCapabilities) can deliver requests before the
	// agent's HTTP server is listening, causing transient failures.
	if s.config.CallbackURL != "" {
		healthURL := strings.TrimRight(s.config.CallbackURL, "/") + "/health"
		client := &http.Client{Timeout: 500 * time.Millisecond}
		for i := 0; i < 20; i++ { // up to 10s
			resp, err := client.Get(healthURL)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					s.logger.Debug("agent callback ready", "url", healthURL, "attempts", i+1)
					break
				}
			}
			time.Sleep(500 * time.Millisecond)
		}
	}

	// Mark as healthy now that the runtime is up.
	s.mu.Lock()
	s.healthy = true
	s.mu.Unlock()

	// Write workspace metadata so hivectl can track agent workspaces.
	// Use a typed struct so started_at marshals as a time.Time (RFC3339),
	// matching the hiveMetadata struct in hivectl/workspace.go.
	if s.config.WorkspacePath != "" {
		type workspaceMetadata struct {
			AgentID   string    `json:"agent_id"`
			Team      string    `json:"team,omitempty"`
			StartedAt time.Time `json:"started_at"`
			Runtime   string    `json:"runtime"`
			Mode      string    `json:"mode"`
			Workspace string    `json:"workspace"`
		}
		metadata := workspaceMetadata{
			AgentID:   s.agentID,
			Team:      s.teamID,
			StartedAt: s.startTime.UTC(),
			Runtime:   s.config.RuntimeCmd,
			Mode:      string(s.config.Mode),
			Workspace: s.config.WorkspacePath,
		}
		metaBytes, _ := json.MarshalIndent(metadata, "", "  ")
		metaPath := filepath.Join(s.config.WorkspacePath, ".hive-metadata.json")
		if err := os.WriteFile(metaPath, metaBytes, 0o600); err != nil {
			s.logger.Warn("failed to write workspace metadata", "error", err)
		}
	}

	return nil
}

// startPostStartup launches background goroutines that run after all core
// subsystems are initialized: heartbeat publishing, lead agent broadcast
// subscription, and runtime monitoring.
func (s *Sidecar) startPostStartup() {
	// Start publishing heartbeats.
	s.startHeartbeat()

	// If this is a lead agent with a callback URL, subscribe to team
	// broadcasts so that trigger messages are forwarded to the agent's
	// /handle/orchestrate endpoint for pipeline orchestration.
	if s.config.IsLead && s.config.CallbackURL != "" && s.teamID != "" {
		if _, err := s.SubscribeTeamBroadcast(func(env types.Envelope) {
			s.logger.Info("lead agent received broadcast, forwarding to orchestrate",
				"from", env.From, "type", env.Type)
			// Forward the envelope payload as inputs to the orchestrate handler.
			var inputs map[string]interface{}
			if len(env.Payload) > 0 {
				if err := json.Unmarshal(env.Payload, &inputs); err != nil {
					inputs = map[string]interface{}{"raw": string(env.Payload)}
				}
			}
			if inputs == nil {
				inputs = map[string]interface{}{}
			}
			result, err := s.forwardToCallback("orchestrate", inputs)
			if err != nil {
				s.logger.Error("orchestrate callback failed", "error", err)
				return
			}
			s.logger.Info("orchestrate completed", "result_keys", len(result))

			// Publish the result to the team result subject so hivectl
			// trigger can display it without scraping logs.
			resultSubj := fmt.Sprintf(protocol.FmtTeamResult, s.teamID)
			resultBytes, err := json.Marshal(result)
			if err == nil {
				s.mu.RLock()
				nc := s.natsConn
				s.mu.RUnlock()
				if nc != nil {
					if pubErr := nc.Publish(resultSubj, resultBytes); pubErr != nil {
						s.logger.Warn("failed to publish pipeline result", "error", pubErr)
					}
				}
			}
		}); err != nil {
			s.logger.Warn("failed to subscribe to team broadcast for lead agent", "error", err)
		}
	}

	// Monitor the runtime; if it exits unexpectedly, mark unhealthy.
	monitorCtx, monitorCancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.monitorCancel = monitorCancel
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.monitorRuntime(monitorCtx)
	}()
}

// monitorRuntime watches the agent runtime process. If the runtime exits,
// the sidecar is marked unhealthy. The goroutine listens to the provided
// context so it exits cleanly during shutdown or when the runtime is
// restarted (the old monitor is cancelled before a new one is launched).
func (s *Sidecar) monitorRuntime(ctx context.Context) {
	// If there is no runtime command, there is nothing to monitor.
	if s.config.RuntimeCmd == "" {
		return
	}

	// Run Wait() in a goroutine so we can select on the context.
	// Track the goroutine with the sidecar's WaitGroup so Stop() waits
	// for it to finish even if the context is not cancelled yet.
	waitCh := make(chan error, 1)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		waitCh <- s.runtime.Wait()
	}()

	select {
	case err := <-waitCh:
		// Runtime exited on its own.
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
	case <-ctx.Done():
		// Monitor cancelled (restart or shutdown); stop monitoring.
		s.logger.Debug("monitorRuntime exiting due to context cancellation")
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

	// Cancel the monitorRuntime goroutine so it exits promptly.
	s.mu.RLock()
	cancel := s.monitorCancel
	s.mu.RUnlock()
	if cancel != nil {
		cancel()
	}

	// Stop the agent runtime BEFORE waiting for goroutines. The monitorRuntime
	// goroutine calls runtime.Wait(), which blocks until the runtime exits.
	// If we called wg.Wait() first, we would deadlock because the monitor
	// goroutine cannot finish until the runtime is stopped.
	if s.runtime != nil {
		if err := s.runtime.Stop(); err != nil {
			s.logger.Error("error stopping runtime", "error", err)
		}
	}

	// Wait for all tracked goroutines (heartbeat, monitorRuntime) to exit
	// before proceeding to drain the NATS connection.
	s.wg.Wait()

	if s.httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.httpServer.Shutdown(ctx); err != nil {
			s.logger.Error("error shutting down HTTP server", "error", err)
		}
	}

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
	s.mu.RLock()
	bcastSub := s.broadcastSub
	s.mu.RUnlock()
	if bcastSub != nil {
		if err := bcastSub.Unsubscribe(); err != nil {
			s.logger.Warn("error unsubscribing from broadcast subject", "error", err)
		}
	}

	s.mu.RLock()
	router := s.capRouter
	s.mu.RUnlock()
	if router != nil {
		router.Stop()
	}

	s.closeNATS()

	if s.vsockProxy != nil {
		s.vsockProxy.Stop()
	}

	s.mu.Lock()
	s.healthy = false
	s.mu.Unlock()

	s.logger.Info("sidecar stopped")
	return nil
}

// defaultCapabilityTimeout is the default time executeCapabilityLocally waits
// for the runtime process to produce a response file before returning an async ack.
const defaultCapabilityTimeout = 30 * time.Second

// capabilityPollInterval is how frequently we check for a response file.
// 200ms balances responsiveness with filesystem pressure.
const capabilityPollInterval = 200 * time.Millisecond

// capabilityTimeout returns the configured capability request timeout,
// falling back to defaultCapabilityTimeout if not set.
func (s *Sidecar) capabilityTimeout() time.Duration {
	if s.config.CapabilityTimeout > 0 {
		return s.config.CapabilityTimeout
	}
	return defaultCapabilityTimeout
}

// capabilityRequest is the JSON structure written to the requests directory
// for the runtime process to pick up and execute.
// SECURITY: All input fields in capability requests are untrusted. Runtimes
// MUST validate inputs before use.
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
// The forwarding mechanism depends on configuration:
//   - CallbackURL set: HTTP POST to {CallbackURL}/handle/{capability}
//   - RuntimeCmd set (no CallbackURL): file-based IPC via workspace requests dir
//   - Neither set: no-op mode (echo inputs back)
func (s *Sidecar) executeCapabilityLocally(capName string, inputs map[string]interface{}) (map[string]interface{}, error) {
	s.logger.Info("executing capability locally",
		"capability", capName,
		"agent_id", s.agentID,
	)

	if s.runtime != nil && !s.runtime.IsRunning() {
		return nil, fmt.Errorf("runtime is not running, cannot execute capability %s", capName)
	}

	// HTTP callback mode: forward to agent process via HTTP.
	if s.config.CallbackURL != "" {
		return s.forwardToCallback(capName, inputs)
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

// forwardToCallback sends a capability invocation to the agent process via
// HTTP POST to {CallbackURL}/handle/{capability}. The agent process must
// respond with {"outputs": {...}} on success or {"error": {...}} on failure.
func (s *Sidecar) forwardToCallback(capName string, inputs map[string]interface{}) (map[string]interface{}, error) {
	url := fmt.Sprintf("%s/handle/%s", strings.TrimRight(s.config.CallbackURL, "/"), capName)

	body := map[string]interface{}{"inputs": inputs}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshalling callback request: %w", err)
	}

	client := &http.Client{Timeout: s.capabilityTimeout()}

	// Retry on connection refused — the agent's HTTP server may still be
	// starting up when the first capability request arrives.
	var resp *http.Response
	for attempt := 0; attempt < 10; attempt++ {
		resp, err = client.Post(url, "application/json", bytes.NewReader(bodyBytes))
		if err == nil {
			break
		}
		if !strings.Contains(err.Error(), "connection refused") {
			break
		}
		s.logger.Debug("agent callback not ready, retrying",
			"capability", capName, "attempt", attempt+1)
		time.Sleep(500 * time.Millisecond)
		// Reset body reader for retry.
		bodyBytes, _ = json.Marshal(body)
	}
	if err != nil {
		return nil, fmt.Errorf("calling agent callback %s: %w", url, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024)) // 10MB limit
	if err != nil {
		return nil, fmt.Errorf("reading callback response: %w", err)
	}

	if resp.StatusCode >= 500 {
		// Agent returned an error.
		var errResp map[string]interface{}
		if json.Unmarshal(respBody, &errResp) == nil {
			return nil, fmt.Errorf("capability %s failed: %v", capName, errResp)
		}
		return nil, fmt.Errorf("capability %s failed with status %d", capName, resp.StatusCode)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parsing callback response: %w", err)
	}

	// If the response has an "outputs" key, return those directly.
	if outputs, ok := result["outputs"]; ok {
		if outputMap, ok := outputs.(map[string]interface{}); ok {
			return outputMap, nil
		}
	}

	return result, nil
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
	if err := writeFileAtomic(reqPath, data, 0600); err != nil {
		return nil, fmt.Errorf("writing capability request file: %w", err)
	}

	s.logger.Info("capability request written, waiting for response",
		"capability", capName,
		"request_id", reqID,
		"request_path", reqPath,
	)

	// Poll for the response file with a timeout.
	deadline := time.After(s.capabilityTimeout())
	ticker := time.NewTicker(capabilityPollInterval)
	defer ticker.Stop()

	const maxResponseSize = 10 * 1024 * 1024 // 10MB

	for {
		// Check for sidecar shutdown so we don't delay the stop sequence.
		select {
		case <-s.stopCh:
			return nil, fmt.Errorf("sidecar shutting down")
		default:
		}

		select {
		case <-s.stopCh:
			return nil, fmt.Errorf("sidecar shutting down")

		case <-deadline:
			// Timeout -- the runtime has not responded yet. Clean up the
			// request and response files to prevent accumulation of stale
			// files in the requests directory.
			s.logger.Warn("capability request timed out waiting for response",
				"capability", capName,
				"request_id", reqID,
			)
			if rmErr := os.Remove(reqPath); rmErr != nil && !os.IsNotExist(rmErr) {
				s.logger.Warn("failed to clean up request file after timeout",
					"path", reqPath, "error", rmErr)
			}
			if rmErr := os.Remove(respPath); rmErr != nil && !os.IsNotExist(rmErr) {
				s.logger.Warn("failed to clean up response file after timeout",
					"path", respPath, "error", rmErr)
			}
			return map[string]interface{}{
				"capability": capName,
				"agent_id":   s.agentID,
				"request_id": reqID,
				"status":     "submitted",
				"message":    "request submitted to runtime, response pending",
			}, nil

		case <-ticker.C:
			// Use os.Open + Stat + LimitReader instead of os.ReadFile to
			// check the file size before reading it into memory.
			f, err := os.Open(respPath)
			if err != nil {
				if os.IsNotExist(err) {
					continue // Not ready yet, keep polling.
				}
				return nil, fmt.Errorf("opening capability response file: %w", err)
			}

			info, err := f.Stat()
			if err != nil {
				f.Close()
				return nil, fmt.Errorf("stat capability response file: %w", err)
			}
			if info.Size() > maxResponseSize {
				f.Close()
				// Clean up both files before returning the error.
				os.Remove(reqPath)
				os.Remove(respPath)
				return nil, fmt.Errorf("response file too large: %d bytes", info.Size())
			}

			respData, err := io.ReadAll(io.LimitReader(f, maxResponseSize+1))
			f.Close()
			if err != nil {
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

// fetchSecrets requests secret values from the hived secrets store via NATS.
func (s *Sidecar) fetchSecrets(names []string) (map[string]string, error) {
	req, err := json.Marshal(struct {
		AgentID string   `json:"agent_id"`
		Names   []string `json:"names"`
	}{
		AgentID: s.agentID,
		Names:   names,
	})
	if err != nil {
		return nil, fmt.Errorf("marshaling secrets request: %w", err)
	}

	msg, err := s.natsConn.Request(protocol.SubjSecretsRequest, req, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("requesting secrets: %w", err)
	}

	var resp struct {
		Secrets map[string]string `json:"secrets"`
		Error   string            `json:"error,omitempty"`
	}
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		return nil, fmt.Errorf("parsing secrets response: %w", err)
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("secrets store error: %s", resp.Error)
	}

	return resp.Secrets, nil
}

// SetEnvVars sets additional environment variables to pass to the runtime process.
// Must be called before Start().
func (s *Sidecar) SetEnvVars(vars []string) {
	s.envVars = vars
}

// IsHealthy returns whether the sidecar and its runtime are both healthy.
func (s *Sidecar) IsHealthy() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.healthy && s.runtime != nil && s.runtime.IsRunning()
}
