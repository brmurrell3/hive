package sidecar

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/brmurrell3/hive/internal/capability"
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

// setupHTTPRoutes registers all HTTP handlers on the provided ServeMux.
func setupHTTPRoutes(mux *http.ServeMux, s *Sidecar) {
	mux.HandleFunc("GET /health", handleHealth(s))
	mux.HandleFunc("GET /capabilities", handleCapabilities(s))
	mux.HandleFunc("POST /capabilities/", handleCapabilityInvoke(s))
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

	// Bind the listener first so we fail fast if the port is taken.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	s.httpServer = &http.Server{
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	s.logger.Info("starting HTTP server", "addr", ln.Addr().String())

	go func() {
		if err := s.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.logger.Error("HTTP server error", "error", err)
		}
	}()

	return nil
}

// handleHealth returns an http.HandlerFunc that reports the sidecar and runtime
// health status along with uptime.
func handleHealth(s *Sidecar) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runtimeStatus := "unhealthy"
		if s.runtime != nil && s.runtime.IsRunning() {
			runtimeStatus = "healthy"
		}

		resp := healthResponse{
			Sidecar:       "healthy",
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

// handleCapabilityInvoke returns an http.HandlerFunc that handles capability
// invocation requests. It bridges HTTP invocations into the NATS capability
// routing system via the sidecar's capability.Router.
//
// It expects paths of the form POST /capabilities/{name}/invoke.
func handleCapabilityInvoke(s *Sidecar) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Parse the capability name from the path.
		// Expected path: /capabilities/{name}/invoke
		path := strings.TrimPrefix(r.URL.Path, "/capabilities/")
		parts := strings.SplitN(path, "/", 2)

		if len(parts) != 2 || parts[1] != "invoke" || parts[0] == "" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		capName := parts[0]

		// Verify the capability exists.
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

		// Decode optional request body.
		var req invokeRequest
		if r.Body != nil && r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"error": "invalid request body: " + err.Error(),
				}, s.logger)
				return
			}
		}

		s.logger.Info("capability invoke via HTTP, executing locally",
			"capability", capName,
			"agent_id", s.agentID,
		)

		// Guard: capability router may be nil if NATS is not yet connected.
		if s.capRouter == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"error": "capability router not ready",
			}, s.logger)
			return
		}

		// Execute the capability locally via the router's registered handler.
		// We use CallLocal instead of Invoke to avoid publishing back to the
		// same NATS subject the router subscribes to, which would cause an
		// infinite loop.
		resp := s.capRouter.CallLocal(capName, req.Inputs)

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
