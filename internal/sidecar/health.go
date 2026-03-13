// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package sidecar

import (
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/brmurrell3/hive/internal/capability"
	"github.com/brmurrell3/hive/internal/protocol"
	"github.com/brmurrell3/hive/internal/types"
)

// Health status constants used in HTTP /health responses and heartbeats.
const (
	statusHealthy   = "healthy"
	statusUnhealthy = "unhealthy"
	statusDegraded  = "degraded"
)

// healthResponse is the JSON structure returned by GET /health.
type healthResponse struct {
	Sidecar       string `json:"sidecar"`
	Runtime       string `json:"runtime"`
	UptimeSeconds int    `json:"uptime_seconds"`
}

// invokeRequest is the JSON body accepted by POST /capabilities/{name}/invoke.
type invokeRequest struct {
	Inputs  map[string]interface{} `json:"inputs,omitempty"`
	Timeout string                 `json:"timeout,omitempty"` // e.g. "30s"
}

// invokeRemoteRequest is the JSON body accepted by POST /capabilities/{name}/invoke-remote.
type invokeRemoteRequest struct {
	Target  string                 `json:"target"` // target agent ID (required)
	Inputs  map[string]interface{} `json:"inputs,omitempty"`
	Timeout string                 `json:"timeout,omitempty"` // e.g. "30s", defaults to "30s"
}

// setupHTTPRoutes registers all HTTP handlers on the provided ServeMux.
func setupHTTPRoutes(mux *http.ServeMux, s *Sidecar) {
	mux.HandleFunc("GET /health", handleHealth(s))
	mux.HandleFunc("GET /capabilities", handleCapabilities(s))
	mux.HandleFunc("GET /team/capabilities", handleTeamCapabilities(s))
	mux.HandleFunc("POST /capabilities/", handleCapabilityRoute(s))
}

// corsMiddleware enforces a same-origin-only policy. No CORS headers are set,
// so browsers enforce same-origin by default. Preflight OPTIONS requests are
// rejected with 403 Forbidden since no external origin is ever allowed.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reject preflight requests since no external origins are allowed.
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusForbidden)
			return
		}

		// No CORS headers = same-origin enforced by browsers.
		next.ServeHTTP(w, r)
	})
}

// authMiddleware returns an http.Handler that enforces Bearer token
// authentication on all endpoints except GET /health (which must remain
// unauthenticated for health checks). If token is empty, all requests
// are passed through without authentication.
func authMiddleware(token string, next http.Handler, logger *slog.Logger) http.Handler {
	if token == "" {
		return next
	}
	tokenBytes := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Allow unauthenticated access to /health for health checks.
		if r.Method == http.MethodGet && r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}

		// Extract Bearer token from Authorization header.
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			logger.Warn("HTTP request missing Authorization header",
				"path", r.URL.Path,
				"method", r.Method,
			)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		const prefix = "Bearer "
		if !strings.HasPrefix(authHeader, prefix) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		provided := []byte(strings.TrimPrefix(authHeader, prefix))
		if subtle.ConstantTimeCompare(provided, tokenBytes) != 1 {
			logger.Warn("HTTP request with invalid Bearer token",
				"path", r.URL.Path,
				"method", r.Method,
			)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// startHTTPServer creates and starts the HTTP server for health and capability
// endpoints. The server listens on the address specified in the sidecar config.
// Verifies the listener actually binds before returning.
func (s *Sidecar) startHTTPServer() error {
	addr := s.config.HTTPAddr
	if addr == "" {
		addr = ":9100"
	}

	mux := http.NewServeMux()
	setupHTTPRoutes(mux, s)

	// Wrap the mux with CORS and authentication middleware.
	var handler http.Handler = mux
	handler = authMiddleware(s.config.HTTPToken, handler, s.logger)
	handler = corsMiddleware(handler)

	// Bind the listener first so we fail fast if the port is taken.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	s.httpServer = &http.Server{
		Handler:           handler,
		ReadTimeout:       10 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8192, // 8KB
	}

	if (s.config.TLSCertFile != "") != (s.config.TLSKeyFile != "") {
		ln.Close() //nolint:errcheck // best-effort cleanup on config error
		return fmt.Errorf("both TLSCertFile and TLSKeyFile must be set, or both must be empty")
	}

	if s.config.TLSCertFile != "" && s.config.TLSKeyFile != "" {
		cert, err := tls.LoadX509KeyPair(s.config.TLSCertFile, s.config.TLSKeyFile)
		if err != nil {
			ln.Close() //nolint:errcheck // best-effort cleanup on TLS error
			return fmt.Errorf("loading sidecar TLS cert/key: %w", err)
		}
		s.httpServer.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
			// Explicit hardened cipher suites for TLS 1.2 connections.
			// TLS 1.3 cipher suites are managed automatically by Go.
			CipherSuites: []uint16{
				tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
				tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
			},
		}
		tlsLn := tls.NewListener(ln, s.httpServer.TLSConfig)
		s.logger.Info("starting HTTPS server", "addr", ln.Addr().String())
		go func() {
			if err := s.httpServer.Serve(tlsLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.logger.Error("HTTPS server error", "error", err)
			}
		}()
	} else {
		s.logger.Info("starting HTTP server", "addr", ln.Addr().String())
		go func() {
			if err := s.httpServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.logger.Error("HTTP server error", "error", err)
			}
		}()
	}

	return nil
}

// handleHealth returns an http.HandlerFunc that reports the sidecar and runtime
// health status along with uptime. The sidecar status reflects the actual state
// of the NATS connection: "healthy" when connected, "degraded" when NATS is
// disconnected, or "unhealthy" when NATS was never established.
func handleHealth(s *Sidecar) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runtimeStatus := statusUnhealthy
		if s.runtime != nil && s.runtime.IsRunning() {
			runtimeStatus = statusHealthy
		}

		// Determine sidecar status based on NATS connectivity.
		// Access natsConn under RLock to synchronize with closeNATS.
		s.mu.RLock()
		nc := s.natsConn
		s.mu.RUnlock()

		sidecarStatus := statusHealthy
		if nc == nil {
			sidecarStatus = statusUnhealthy
		} else if !nc.IsConnected() {
			sidecarStatus = statusDegraded
		}

		resp := healthResponse{
			Sidecar:       sidecarStatus,
			Runtime:       runtimeStatus,
			UptimeSeconds: int(time.Since(s.startTime).Seconds()),
		}

		writeJSON(w, http.StatusOK, resp, s.logger)
	}
}

// handleCapabilities returns an http.HandlerFunc that responds with the list of
// capabilities configured for this agent.
func handleCapabilities(s *Sidecar) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.RLock()
		caps := s.capabilities
		s.mu.RUnlock()

		if caps == nil {
			caps = []Capability{}
		}

		writeJSON(w, http.StatusOK, caps, s.logger)
	}
}

// teamCapabilityEntry is the JSON structure returned by GET /team/capabilities
// for each capability registered across all teams.
type teamCapabilityEntry struct {
	Name        string `json:"name"`
	AgentID     string `json:"agent_id"`
	TeamID      string `json:"team_id"`
	Description string `json:"description,omitempty"`
}

// handleTeamCapabilities returns an http.HandlerFunc that queries the control
// plane for all registered capabilities across all teams via NATS and returns
// them as a JSON array. This enables cross-team capability discovery.
func handleTeamCapabilities(s *Sidecar) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Get NATS connection under RLock.
		s.mu.RLock()
		nc := s.natsConn
		s.mu.RUnlock()

		if nc == nil || !nc.IsConnected() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"error": "NATS connection not available",
			}, s.logger)
			return
		}

		// Build an envelope wrapping a CtlRequest, matching the format
		// used by hivectl's DaemonClient.request().
		payloadBytes, err := json.Marshal(&protocol.CtlRequest{})
		if err != nil {
			s.logger.Error("failed to marshal capabilities request payload", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error": "internal server error",
			}, s.logger)
			return
		}

		env := types.Envelope{
			ID:        types.NewUUID(),
			From:      s.agentID,
			To:        "hived",
			Type:      types.MessageTypeControl,
			Timestamp: time.Now().UTC(),
			Payload:   payloadBytes,
		}

		data, err := json.Marshal(env)
		if err != nil {
			s.logger.Error("failed to marshal capabilities request envelope", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error": "internal server error",
			}, s.logger)
			return
		}

		// Send NATS request with 5s timeout.
		msg, err := nc.Request(protocol.SubjCapabilityList, data, 5*time.Second)
		if err != nil {
			s.logger.Warn("failed to query capabilities from control plane", "error", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{
				"error": "failed to query control plane",
			}, s.logger)
			return
		}

		// Parse the CtlResponse.
		var resp protocol.CtlResponse
		if err := json.Unmarshal(msg.Data, &resp); err != nil {
			s.logger.Warn("failed to parse capabilities response", "error", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{
				"error": "invalid response from control plane",
			}, s.logger)
			return
		}
		if !resp.Success {
			s.logger.Warn("control plane returned error for capabilities list", "error", resp.Error)
			writeJSON(w, http.StatusBadGateway, map[string]string{
				"error": "control plane error",
			}, s.logger)
			return
		}

		// Parse the Data field which contains the capabilities array.
		// The server returns []capEntry with fields: name, agent_id, team.
		// We transform to our response format with team_id and description.
		var serverCaps []struct {
			Name        string `json:"name"`
			AgentID     string `json:"agent_id"`
			Team        string `json:"team"`
			Description string `json:"description"`
		}
		if err := json.Unmarshal(resp.Data, &serverCaps); err != nil {
			s.logger.Warn("failed to parse capabilities data", "error", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{
				"error": "invalid capabilities data from control plane",
			}, s.logger)
			return
		}

		// Transform to the desired response format.
		result := make([]teamCapabilityEntry, len(serverCaps))
		for i, cap := range serverCaps {
			result[i] = teamCapabilityEntry{
				Name:        cap.Name,
				AgentID:     cap.AgentID,
				TeamID:      cap.Team,
				Description: cap.Description,
			}
		}

		writeJSON(w, http.StatusOK, result, s.logger)
	}
}

// handleCapabilityRoute returns an http.HandlerFunc that dispatches capability
// requests to either local invoke or remote invoke handlers based on the URL path.
//
// It expects paths of the form:
//   - POST /capabilities/{name}/invoke        — invoke locally
//   - POST /capabilities/{name}/invoke-remote  — invoke on a remote agent via NATS
func handleCapabilityRoute(s *Sidecar) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Limit request body size to 1MB to prevent resource exhaustion.
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB limit

		// Parse the capability name and action from the path.
		path := strings.TrimPrefix(r.URL.Path, "/capabilities/")
		parts := strings.SplitN(path, "/", 2)

		if len(parts) != 2 || parts[0] == "" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		capName := parts[0]
		action := parts[1]

		switch action {
		case "invoke":
			handleCapabilityInvoke(s, w, r, capName)
		case "invoke-remote":
			handleCapabilityInvokeRemote(s, w, r, capName)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}
}

// handleCapabilityInvoke handles local capability invocation requests.
// It bridges HTTP invocations into the NATS capability routing system
// via the sidecar's capability.Router.
func handleCapabilityInvoke(s *Sidecar, w http.ResponseWriter, r *http.Request, capName string) {
	// Verify the capability exists on this agent.
	s.mu.RLock()
	found := false
	for _, c := range s.capabilities {
		if c.Name == capName {
			found = true
			break
		}
	}
	s.mu.RUnlock()

	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "capability not found",
			"name":  capName,
		}, s.logger)
		return
	}

	// Decode optional request body. Skip decoding only when ContentLength
	// is exactly 0 (explicitly empty). When ContentLength is -1 (unknown /
	// chunked transfer encoding), read the body with a size-limited reader
	// to handle chunked requests properly. MaxBytesReader already protects
	// against oversized payloads.
	var req invokeRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			// If the body was chunked but empty, the decoder returns EOF
			// which is not an error worth rejecting.
			if !errors.Is(err, io.EOF) {
				s.logger.Warn("invalid request body", "error", err)
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"error": "invalid request format",
				}, s.logger)
				return
			}
		}
	}

	s.logger.Info("capability invoke via HTTP, executing locally",
		"capability", capName,
		"agent_id", s.agentID,
	)

	// Guard: capability router may be nil if NATS is not yet connected.
	s.mu.RLock()
	router := s.capRouter
	s.mu.RUnlock()
	if router == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "capability router not ready",
		}, s.logger)
		return
	}

	// Execute the capability locally via the router's registered handler.
	resp := router.CallLocal(capName, req.Inputs)

	// Map the capability.InvocationResponse to a friendly HTTP response.
	httpResp := buildInvokeHTTPResponse(resp)
	status := http.StatusOK
	if resp.Status == "error" {
		status = http.StatusUnprocessableEntity
	} else if resp.Status == "timeout" {
		status = http.StatusGatewayTimeout
	}
	writeJSON(w, status, httpResp, s.logger)
}

// handleCapabilityInvokeRemote handles remote capability invocation requests.
// It invokes a capability on a different agent via NATS using router.Invoke().
func handleCapabilityInvokeRemote(s *Sidecar, w http.ResponseWriter, r *http.Request, capName string) {
	var req invokeRemoteRequest
	if r.Body == nil || r.ContentLength == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "request body required",
		}, s.logger)
		return
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.logger.Warn("invalid invoke-remote request body", "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid request format",
		}, s.logger)
		return
	}

	if req.Target == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "target field is required",
		}, s.logger)
		return
	}

	// Guard: capability router may be nil if NATS is not yet connected.
	s.mu.RLock()
	router := s.capRouter
	s.mu.RUnlock()
	if router == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "capability router not ready",
		}, s.logger)
		return
	}

	// Parse timeout, default to 30s.
	timeout := 30 * time.Second
	if req.Timeout != "" {
		if parsed, err := time.ParseDuration(req.Timeout); err == nil {
			timeout = parsed
		}
	}

	s.logger.Info("invoke-remote via HTTP",
		"capability", capName,
		"target", req.Target,
		"agent_id", s.agentID,
	)

	resp, err := router.Invoke(req.Target, capName, req.Inputs, timeout)
	if err != nil {
		// Determine if the error indicates the agent is offline.
		errMsg := err.Error()
		code := "INTERNAL_ERROR"
		httpStatus := http.StatusBadGateway
		if strings.Contains(errMsg, "timeout") || strings.Contains(errMsg, "no responders") {
			code = "AGENT_OFFLINE"
			httpStatus = http.StatusGatewayTimeout
		}
		writeJSON(w, httpStatus, map[string]interface{}{
			"status": "error",
			"error": map[string]interface{}{
				"code":      code,
				"message":   errMsg,
				"retryable": code == "AGENT_OFFLINE",
			},
		}, s.logger)
		return
	}

	httpResp := buildInvokeHTTPResponse(resp)
	status := http.StatusOK
	if resp.Status == "error" {
		status = http.StatusUnprocessableEntity
	} else if resp.Status == "timeout" {
		status = http.StatusGatewayTimeout
	}
	writeJSON(w, status, httpResp, s.logger)
}

// buildInvokeHTTPResponse converts an InvocationResponse into the JSON body
// returned to the HTTP caller.
func buildInvokeHTTPResponse(resp *capability.InvocationResponse) map[string]interface{} {
	out := map[string]interface{}{
		"status":      resp.Status,
		"duration_ms": resp.DurationMs,
	}
	if resp.Outputs != nil {
		out["outputs"] = resp.Outputs
	}
	if resp.Error != nil {
		out["error"] = map[string]interface{}{
			"code":      resp.Error.Code,
			"message":   resp.Error.Message,
			"retryable": resp.Error.Retryable,
		}
	}
	return out
}

// writeJSON marshals v to JSON and writes it to the response with the given
// status code. On marshal failure, it returns a 500 error.
func writeJSON(w http.ResponseWriter, status int, v interface{}, logger *slog.Logger) {
	data, err := json.Marshal(v)
	if err != nil {
		logger.Error("failed to marshal JSON response", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(data); err != nil {
		logger.Error("failed to write HTTP response", "error", err)
	}
}
