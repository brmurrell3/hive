// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

// Package hive provides a Go SDK for building Hive agents.
//
// An agent registers capability handlers and starts an HTTP server that the
// Hive sidecar calls to invoke those capabilities. The SDK reads all
// configuration from environment variables set by the Hive runtime.
//
// Basic usage:
//
//	agent := hive.NewAgent()
//	agent.HandleCapability("greet", func(inputs map[string]any) (map[string]any, error) {
//	    name := inputs["name"].(string)
//	    return map[string]any{"message": "Hello, " + name + "!"}, nil
//	})
//	agent.Run(context.Background())
package hive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// CapabilityHandler is the function signature for capability handlers.
// It receives a map of input parameters and returns a map of output values.
// Returning a non-nil error causes the sidecar to receive an error response
// with code "CAPABILITY_FAILED".
type CapabilityHandler func(inputs map[string]any) (map[string]any, error)

// Agent is the core type for a Hive agent. It manages capability registration,
// an HTTP server for the sidecar to call, and provides methods for invoking
// remote capabilities.
type Agent struct {
	agentID    string
	teamID     string
	sidecarURL string
	workspace  string
	port       string
	logger     *slog.Logger

	mu           sync.RWMutex
	capabilities map[string]CapabilityHandler

	// httpClient is used for outbound requests to the sidecar.
	httpClient *http.Client
}

// config holds the environment-derived configuration.
type config struct {
	agentID    string
	teamID     string
	sidecarURL string
	workspace  string
	port       string
}

// readConfig reads agent configuration from environment variables.
func readConfig() config {
	port := os.Getenv("HIVE_CALLBACK_PORT")
	if port == "" {
		port = "9200"
	}
	return config{
		agentID:    os.Getenv("HIVE_AGENT_ID"),
		teamID:     os.Getenv("HIVE_TEAM_ID"),
		sidecarURL: os.Getenv("HIVE_SIDECAR_URL"),
		workspace:  os.Getenv("HIVE_WORKSPACE"),
		port:       port,
	}
}

// NewAgent creates a new Agent, reading configuration from environment
// variables:
//   - HIVE_CALLBACK_PORT: HTTP listen port (default "9200")
//   - HIVE_AGENT_ID: agent identifier
//   - HIVE_TEAM_ID: team identifier
//   - HIVE_SIDECAR_URL: sidecar API base URL (e.g., "http://127.0.0.1:9100")
//   - HIVE_WORKSPACE: workspace directory
func NewAgent() *Agent {
	cfg := readConfig()
	return newAgentFromConfig(cfg)
}

// newAgentFromConfig creates an Agent from an explicit config (used by tests).
func newAgentFromConfig(cfg config) *Agent {
	return &Agent{
		agentID:      cfg.agentID,
		teamID:       cfg.teamID,
		sidecarURL:   cfg.sidecarURL,
		workspace:    cfg.workspace,
		port:         cfg.port,
		logger:       slog.Default(),
		capabilities: make(map[string]CapabilityHandler),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// AgentID returns the agent's identifier (from HIVE_AGENT_ID).
func (a *Agent) AgentID() string {
	return a.agentID
}

// TeamID returns the agent's team identifier (from HIVE_TEAM_ID).
func (a *Agent) TeamID() string {
	return a.teamID
}

// Workspace returns the agent's workspace directory (from HIVE_WORKSPACE).
func (a *Agent) Workspace() string {
	return a.workspace
}

// HandleCapability registers a handler for the named capability.
// The handler will be invoked when the sidecar sends
// POST /handle/{capability_name} to this agent.
//
// Panics if name is empty or handler is nil.
func (a *Agent) HandleCapability(name string, handler CapabilityHandler) {
	if name == "" {
		panic("hive: capability name must not be empty")
	}
	if handler == nil {
		panic("hive: capability handler must not be nil")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.capabilities[name] = handler
}

// invokeRequest is the JSON body sent to the sidecar's invoke-remote endpoint.
type invokeRequest struct {
	Target  string         `json:"target"`
	Inputs  map[string]any `json:"inputs,omitempty"`
	Timeout string         `json:"timeout,omitempty"`
}

// invokeResponse is the JSON body returned by a successful invoke-remote call.
type invokeResponse struct {
	Outputs map[string]any `json:"outputs,omitempty"`
	Error   *invokeError   `json:"error,omitempty"`
}

// invokeError represents an error response from the sidecar or remote agent.
type invokeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Invoke calls a capability on a remote agent via the sidecar's invoke-remote
// endpoint. It POSTs to {HIVE_SIDECAR_URL}/capabilities/{capability}/invoke-remote
// with the target agent ID and inputs.
//
// Returns the output map on success, or an error if the request fails or the
// remote agent returns an error response.
func (a *Agent) Invoke(ctx context.Context, target, capability string, inputs map[string]any) (map[string]any, error) {
	if a.sidecarURL == "" {
		return nil, fmt.Errorf("hive: HIVE_SIDECAR_URL not set")
	}
	if target == "" {
		return nil, fmt.Errorf("hive: target agent ID must not be empty")
	}
	if capability == "" {
		return nil, fmt.Errorf("hive: capability name must not be empty")
	}

	reqBody := invokeRequest{
		Target: target,
		Inputs: inputs,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("hive: marshal invoke request: %w", err)
	}

	url := strings.TrimRight(a.sidecarURL, "/") + "/capabilities/" + capability + "/invoke-remote"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("hive: create invoke request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hive: invoke %s on %s: %w", capability, target, err)
	}
	defer resp.Body.Close()

	// Limit response body to 2MB to prevent resource exhaustion.
	const maxResponseBody = 2 << 20
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("hive: read invoke response: %w", err)
	}

	var result invokeResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("hive: decode invoke response (status %d): %w", resp.StatusCode, err)
	}

	if result.Error != nil {
		return nil, fmt.Errorf("hive: invoke %s on %s: [%s] %s", capability, target, result.Error.Code, result.Error.Message)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hive: invoke %s on %s: unexpected status %d", capability, target, resp.StatusCode)
	}

	return result.Outputs, nil
}

// handleRequest is the JSON body received on POST /handle/{capability_name}.
type handleRequest struct {
	Inputs map[string]any `json:"inputs"`
}

// Run starts the agent's HTTP server and blocks until the context is cancelled
// or SIGTERM is received. It performs a graceful shutdown with a 5-second
// timeout.
func (a *Agent) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", a.handleHealth)
	mux.HandleFunc("/handle/", a.handleCapability)

	addr := net.JoinHostPort("127.0.0.1", a.port)
	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1MB
	}

	// Create a context that is cancelled on SIGTERM or parent cancellation.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Start the server in a goroutine.
	errCh := make(chan error, 1)
	go func() {
		a.logger.Info("hive agent starting",
			"addr", addr,
			"agent_id", a.agentID,
			"team_id", a.teamID,
		)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	// Block until context done or server error.
	select {
	case <-ctx.Done():
		a.logger.Info("shutting down agent", "reason", ctx.Err())
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("hive: server error: %w", err)
		}
		return nil
	}

	// Graceful shutdown with a 5-second deadline.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("hive: shutdown error: %w", err)
	}

	a.logger.Info("agent stopped")
	return nil
}

// handleHealth responds to GET /health with a status JSON.
func (a *Agent) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "healthy"})
}

// handleCapability routes POST /handle/{capability_name} to the registered handler.
func (a *Agent) handleCapability(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract capability name from path: /handle/{name}
	capName := strings.TrimPrefix(r.URL.Path, "/handle/")
	if capName == "" || capName == r.URL.Path {
		writeErrorJSON(w, http.StatusNotFound, "CAPABILITY_NOT_FOUND", "no capability name in path")
		return
	}

	a.mu.RLock()
	handler, ok := a.capabilities[capName]
	a.mu.RUnlock()

	if !ok {
		writeErrorJSON(w, http.StatusNotFound, "CAPABILITY_NOT_FOUND",
			fmt.Sprintf("no handler registered for capability %q", capName))
		return
	}

	// Limit request body to 1MB.
	const maxRequestBody = 1 << 20
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
	if err != nil {
		writeErrorJSON(w, http.StatusBadRequest, "INVALID_REQUEST", "failed to read request body")
		return
	}

	var req handleRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeErrorJSON(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid JSON in request body")
			return
		}
	}

	if req.Inputs == nil {
		req.Inputs = make(map[string]any)
	}

	// Call the handler.
	outputs, handlerErr := handler(req.Inputs)
	if handlerErr != nil {
		a.logger.Warn("capability handler failed",
			"capability", capName,
			"error", handlerErr,
		)
		writeErrorJSON(w, http.StatusInternalServerError, "CAPABILITY_FAILED", handlerErr.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"outputs": outputs})
}

// writeErrorJSON writes a JSON error response.
func writeErrorJSON(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
}
