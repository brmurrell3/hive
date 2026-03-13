// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package hive

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// NewAgent / environment variable reading
// ---------------------------------------------------------------------------

func TestNewAgent_DefaultPort(t *testing.T) {
	// Clear any existing env vars that could interfere.
	t.Setenv("HIVE_CALLBACK_PORT", "")
	t.Setenv("HIVE_AGENT_ID", "")
	t.Setenv("HIVE_TEAM_ID", "")
	t.Setenv("HIVE_SIDECAR_URL", "")
	t.Setenv("HIVE_WORKSPACE", "")

	a := NewAgent()
	if a.port != "9200" {
		t.Errorf("expected default port 9200, got %s", a.port)
	}
}

func TestNewAgent_EnvVars(t *testing.T) {
	t.Setenv("HIVE_CALLBACK_PORT", "8888")
	t.Setenv("HIVE_AGENT_ID", "test-agent")
	t.Setenv("HIVE_TEAM_ID", "test-team")
	t.Setenv("HIVE_SIDECAR_URL", "http://127.0.0.1:9100")
	t.Setenv("HIVE_WORKSPACE", "/tmp/workspace")

	a := NewAgent()
	if a.port != "8888" {
		t.Errorf("expected port 8888, got %s", a.port)
	}
	if a.AgentID() != "test-agent" {
		t.Errorf("expected agent_id test-agent, got %s", a.AgentID())
	}
	if a.TeamID() != "test-team" {
		t.Errorf("expected team_id test-team, got %s", a.TeamID())
	}
	if a.Workspace() != "/tmp/workspace" {
		t.Errorf("expected workspace /tmp/workspace, got %s", a.Workspace())
	}
}

// ---------------------------------------------------------------------------
// Capability registration
// ---------------------------------------------------------------------------

func TestHandleCapability_Registration(t *testing.T) {
	a := newTestAgent()
	a.HandleCapability("greet", func(inputs map[string]any) (map[string]any, error) {
		return map[string]any{"msg": "hello"}, nil
	})

	a.mu.RLock()
	_, ok := a.capabilities["greet"]
	a.mu.RUnlock()
	if !ok {
		t.Fatal("expected capability 'greet' to be registered")
	}
}

func TestHandleCapability_PanicsOnEmptyName(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for empty capability name")
		}
	}()

	a := newTestAgent()
	a.HandleCapability("", func(inputs map[string]any) (map[string]any, error) {
		return nil, nil
	})
}

func TestHandleCapability_PanicsOnNilHandler(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for nil handler")
		}
	}()

	a := newTestAgent()
	a.HandleCapability("test", nil)
}

// ---------------------------------------------------------------------------
// Health endpoint
// ---------------------------------------------------------------------------

func TestHealthEndpoint(t *testing.T) {
	a := newTestAgent()
	mux := testMux(a)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp["status"] != "healthy" {
		t.Errorf("expected status healthy, got %s", resp["status"])
	}
}

func TestHealthEndpoint_MethodNotAllowed(t *testing.T) {
	a := newTestAgent()
	mux := testMux(a)

	req := httptest.NewRequest(http.MethodPost, "/health", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status 405, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Capability invocation (sidecar -> agent HTTP)
// ---------------------------------------------------------------------------

func TestHandleCapability_Success(t *testing.T) {
	a := newTestAgent()
	a.HandleCapability("greet", func(inputs map[string]any) (map[string]any, error) {
		name, _ := inputs["name"].(string)
		return map[string]any{"message": "Hello, " + name + "!"}, nil
	})
	mux := testMux(a)

	body := `{"inputs": {"name": "World"}}`
	req := httptest.NewRequest(http.MethodPost, "/handle/greet", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	outputs, ok := resp["outputs"].(map[string]any)
	if !ok {
		t.Fatal("expected outputs in response")
	}
	if outputs["message"] != "Hello, World!" {
		t.Errorf("expected 'Hello, World!', got %v", outputs["message"])
	}
}

func TestHandleCapability_EmptyInputs(t *testing.T) {
	a := newTestAgent()
	a.HandleCapability("ping", func(inputs map[string]any) (map[string]any, error) {
		return map[string]any{"pong": true}, nil
	})
	mux := testMux(a)

	// Send with empty body.
	req := httptest.NewRequest(http.MethodPost, "/handle/ping", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleCapability_HandlerError(t *testing.T) {
	a := newTestAgent()
	a.HandleCapability("fail", func(inputs map[string]any) (map[string]any, error) {
		return nil, fmt.Errorf("something went wrong")
	})
	mux := testMux(a)

	body := `{"inputs": {}}`
	req := httptest.NewRequest(http.MethodPost, "/handle/fail", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected status 500, got %d", w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatal("expected error object in response")
	}
	if errObj["code"] != "CAPABILITY_FAILED" {
		t.Errorf("expected code CAPABILITY_FAILED, got %v", errObj["code"])
	}
	if errObj["message"] != "something went wrong" {
		t.Errorf("expected message 'something went wrong', got %v", errObj["message"])
	}
}

func TestHandleCapability_NotFound(t *testing.T) {
	a := newTestAgent()
	mux := testMux(a)

	body := `{"inputs": {}}`
	req := httptest.NewRequest(http.MethodPost, "/handle/nonexistent", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected status 404, got %d", w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatal("expected error object in response")
	}
	if errObj["code"] != "CAPABILITY_NOT_FOUND" {
		t.Errorf("expected code CAPABILITY_NOT_FOUND, got %v", errObj["code"])
	}
}

func TestHandleCapability_InvalidJSON(t *testing.T) {
	a := newTestAgent()
	a.HandleCapability("test", func(inputs map[string]any) (map[string]any, error) {
		return map[string]any{}, nil
	})
	mux := testMux(a)

	req := httptest.NewRequest(http.MethodPost, "/handle/test", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", w.Code)
	}
}

func TestHandleCapability_MethodNotAllowed(t *testing.T) {
	a := newTestAgent()
	a.HandleCapability("test", func(inputs map[string]any) (map[string]any, error) {
		return map[string]any{}, nil
	})
	mux := testMux(a)

	req := httptest.NewRequest(http.MethodGet, "/handle/test", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected status 405, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Invoke (agent -> sidecar -> remote agent)
// ---------------------------------------------------------------------------

func TestInvoke_Success(t *testing.T) {
	// Set up a mock sidecar server.
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		expectedPath := "/capabilities/greet/invoke-remote"
		if r.URL.Path != expectedPath {
			t.Errorf("expected path %s, got %s", expectedPath, r.URL.Path)
		}

		var req invokeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Target != "agent-b" {
			t.Errorf("expected target agent-b, got %s", req.Target)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"outputs": map[string]any{"message": "Hello!"},
		})
	}))
	defer sidecar.Close()

	a := newTestAgent()
	a.sidecarURL = sidecar.URL

	outputs, err := a.Invoke(context.Background(), "agent-b", "greet", map[string]any{"name": "World"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outputs["message"] != "Hello!" {
		t.Errorf("expected 'Hello!', got %v", outputs["message"])
	}
}

func TestInvoke_RemoteError(t *testing.T) {
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"code":    "CAPABILITY_FAILED",
				"message": "handler crashed",
			},
		})
	}))
	defer sidecar.Close()

	a := newTestAgent()
	a.sidecarURL = sidecar.URL

	_, err := a.Invoke(context.Background(), "agent-b", "greet", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "CAPABILITY_FAILED") {
		t.Errorf("expected error to contain CAPABILITY_FAILED, got: %v", err)
	}
}

func TestInvoke_NoSidecarURL(t *testing.T) {
	a := newTestAgent()
	a.sidecarURL = ""

	_, err := a.Invoke(context.Background(), "agent-b", "greet", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "HIVE_SIDECAR_URL not set") {
		t.Errorf("expected HIVE_SIDECAR_URL error, got: %v", err)
	}
}

func TestInvoke_EmptyTarget(t *testing.T) {
	a := newTestAgent()
	a.sidecarURL = "http://127.0.0.1:9100"

	_, err := a.Invoke(context.Background(), "", "greet", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "target agent ID must not be empty") {
		t.Errorf("expected target error, got: %v", err)
	}
}

func TestInvoke_EmptyCapability(t *testing.T) {
	a := newTestAgent()
	a.sidecarURL = "http://127.0.0.1:9100"

	_, err := a.Invoke(context.Background(), "agent-b", "", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "capability name must not be empty") {
		t.Errorf("expected capability error, got: %v", err)
	}
}

func TestInvoke_ContextCancellation(t *testing.T) {
	// Use a listener that we never accept on — the client will connect but
	// the request will hang until the context deadline fires.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	a := newTestAgent()
	a.sidecarURL = "http://" + ln.Addr().String()
	// Use a client without its own timeout so only the context controls it.
	a.httpClient = &http.Client{}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, invokeErr := a.Invoke(ctx, "agent-b", "greet", nil)
	if invokeErr == nil {
		t.Fatal("expected error from cancelled context")
	}
}

// ---------------------------------------------------------------------------
// Run / graceful shutdown
// ---------------------------------------------------------------------------

func TestRun_GracefulShutdown(t *testing.T) {
	a := newTestAgentWithPort(t)
	a.HandleCapability("test", func(inputs map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- a.Run(ctx)
	}()

	// Give the server time to start.
	time.Sleep(100 * time.Millisecond)

	// Verify the server is responding.
	resp, err := http.Get("http://127.0.0.1:" + a.port + "/health")
	if err != nil {
		t.Fatalf("health check failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Cancel context to trigger shutdown.
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5 seconds")
	}
}

func TestRun_ServesCapabilities(t *testing.T) {
	a := newTestAgentWithPort(t)
	a.HandleCapability("echo", func(inputs map[string]any) (map[string]any, error) {
		return inputs, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- a.Run(ctx)
	}()

	// Give the server time to start.
	time.Sleep(100 * time.Millisecond)

	// Invoke the capability via HTTP.
	body := strings.NewReader(`{"inputs": {"msg": "hello"}}`)
	resp, err := http.Post("http://127.0.0.1:"+a.port+"/handle/echo", "application/json", body)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	outputs, ok := result["outputs"].(map[string]any)
	if !ok {
		t.Fatal("expected outputs in response")
	}
	if outputs["msg"] != "hello" {
		t.Errorf("expected 'hello', got %v", outputs["msg"])
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5 seconds")
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// newTestAgent creates an Agent with test defaults (no env dependency).
func newTestAgent() *Agent {
	return newAgentFromConfig(config{
		agentID:    "test-agent",
		teamID:     "test-team",
		sidecarURL: "http://127.0.0.1:9100",
		workspace:  "/tmp/test-workspace",
		port:       "9200",
	})
}

// newTestAgentWithPort creates an Agent bound to a random available port.
// The port is allocated by briefly listening, then immediately closed so
// the agent can bind to it.
func newTestAgentWithPort(t *testing.T) *Agent {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate test port: %v", err)
	}
	port := fmt.Sprintf("%d", ln.Addr().(*net.TCPAddr).Port)
	ln.Close()

	return newAgentFromConfig(config{
		agentID:    "test-agent",
		teamID:     "test-team",
		sidecarURL: "http://127.0.0.1:9100",
		workspace:  "/tmp/test-workspace",
		port:       port,
	})
}

// testMux creates an http.ServeMux wired to the agent's handlers (for
// httptest usage without starting a real server).
func testMux(a *Agent) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", a.handleHealth)
	mux.HandleFunc("/handle/", a.handleCapability)
	return mux
}
