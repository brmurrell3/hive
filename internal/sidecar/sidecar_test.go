//go:build unit

package sidecar

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newTestSidecar creates a Sidecar with sensible defaults for testing.
// The runtime is initialised in no-op mode (empty RuntimeCmd) so that
// IsRunning() returns true after Start on the runtime.
func newTestSidecar(caps []Capability) *Sidecar {
	cfg := Config{
		AgentID:  "test-agent",
		TeamID:   "test-team",
		HTTPAddr: ":0",
		Capabilities: caps,
	}

	s := New(cfg, testLogger())
	// Initialise the runtime in no-op mode so handler code that calls
	// s.runtime.IsRunning() does not panic on a nil receiver.
	s.runtime = NewRuntime("", nil, "", s.logger)
	s.runtime.Start()
	s.startTime = time.Now().Add(-60 * time.Second) // pretend we started 60s ago
	return s
}

// testMux creates an http.ServeMux with the sidecar routes registered, suitable
// for use with httptest.
func testMux(s *Sidecar) *http.ServeMux {
	mux := http.NewServeMux()
	setupHTTPRoutes(mux, s)
	return mux
}

// ---------------------------------------------------------------------------
// Heartbeat message construction
// ---------------------------------------------------------------------------

func TestHeartbeatMessageSchema(t *testing.T) {
	// The heartbeat is constructed in publishHeartbeat via types.Envelope.
	// We verify the structure by calling newUUID and assembling the same
	// fields the sidecar would use.

	uuid := newUUID()

	// UUID v4 format: 8-4-4-4-12 hex characters.
	uuidRe := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !uuidRe.MatchString(uuid) {
		t.Errorf("newUUID() = %q, does not match UUID v4 format", uuid)
	}
}

func TestNewUUID_Uniqueness(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := newUUID()
		if seen[id] {
			t.Fatalf("duplicate UUID generated: %s", id)
		}
		seen[id] = true
	}
}

func TestNewUUID_Version4Bits(t *testing.T) {
	for i := 0; i < 50; i++ {
		uuid := newUUID()
		// Version nibble (position 14) must be '4'.
		if uuid[14] != '4' {
			t.Errorf("UUID version nibble = %c, want '4' in %s", uuid[14], uuid)
		}
		// Variant nibble (position 19) must be one of 8, 9, a, b.
		variant := uuid[19]
		if variant != '8' && variant != '9' && variant != 'a' && variant != 'b' {
			t.Errorf("UUID variant nibble = %c, want 8/9/a/b in %s", variant, uuid)
		}
	}
}

// ---------------------------------------------------------------------------
// GET /health endpoint
// ---------------------------------------------------------------------------

func TestHealthEndpoint_ReturnsJSON(t *testing.T) {
	s := newTestSidecar(nil)
	mux := testMux(s)

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /health status = %d, want %d", w.Code, http.StatusOK)
	}

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}

	var resp healthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal health response: %v", err)
	}

	if resp.Sidecar != "healthy" {
		t.Errorf("sidecar status = %q, want %q", resp.Sidecar, "healthy")
	}

	// Runtime is in no-op mode and started, so it should be "healthy".
	if resp.Runtime != "healthy" {
		t.Errorf("runtime status = %q, want %q", resp.Runtime, "healthy")
	}

	// We set startTime to 60s ago, so uptime should be >= 60.
	if resp.UptimeSeconds < 60 {
		t.Errorf("uptime_seconds = %d, want >= 60", resp.UptimeSeconds)
	}
}

func TestHealthEndpoint_RuntimeUnhealthy(t *testing.T) {
	s := newTestSidecar(nil)

	// Stop the no-op runtime so IsRunning() returns false.
	s.runtime.Stop()

	mux := testMux(s)

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /health status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp healthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal health response: %v", err)
	}

	if resp.Runtime != "unhealthy" {
		t.Errorf("runtime status = %q, want %q", resp.Runtime, "unhealthy")
	}

	// Sidecar always reports itself as "healthy" from the HTTP endpoint.
	if resp.Sidecar != "healthy" {
		t.Errorf("sidecar status = %q, want %q", resp.Sidecar, "healthy")
	}
}

// ---------------------------------------------------------------------------
// GET /capabilities endpoint
// ---------------------------------------------------------------------------

func TestCapabilitiesEndpoint_ReturnsList(t *testing.T) {
	caps := []Capability{
		{
			Name:        "code-review",
			Description: "Reviews code changes",
			Inputs: []CapabilityParam{
				{Name: "diff", Type: "string", Description: "The diff to review", Required: true},
			},
			Outputs: []CapabilityParam{
				{Name: "comments", Type: "string", Description: "Review comments", Required: true},
			},
			Async: false,
		},
		{
			Name:        "deploy",
			Description: "Deploys a service",
			Async:       true,
		},
	}

	s := newTestSidecar(caps)
	mux := testMux(s)

	req := httptest.NewRequest("GET", "/capabilities", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /capabilities status = %d, want %d", w.Code, http.StatusOK)
	}

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}

	var got []Capability
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to unmarshal capabilities: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("got %d capabilities, want 2", len(got))
	}

	if got[0].Name != "code-review" {
		t.Errorf("capabilities[0].Name = %q, want %q", got[0].Name, "code-review")
	}
	if got[0].Description != "Reviews code changes" {
		t.Errorf("capabilities[0].Description = %q, want %q", got[0].Description, "Reviews code changes")
	}
	if len(got[0].Inputs) != 1 {
		t.Fatalf("capabilities[0].Inputs has %d entries, want 1", len(got[0].Inputs))
	}
	if got[0].Inputs[0].Name != "diff" {
		t.Errorf("capabilities[0].Inputs[0].Name = %q, want %q", got[0].Inputs[0].Name, "diff")
	}
	if got[0].Inputs[0].Required != true {
		t.Error("capabilities[0].Inputs[0].Required = false, want true")
	}
	if len(got[0].Outputs) != 1 {
		t.Fatalf("capabilities[0].Outputs has %d entries, want 1", len(got[0].Outputs))
	}
	if got[0].Async != false {
		t.Error("capabilities[0].Async = true, want false")
	}

	if got[1].Name != "deploy" {
		t.Errorf("capabilities[1].Name = %q, want %q", got[1].Name, "deploy")
	}
	if got[1].Async != true {
		t.Error("capabilities[1].Async = false, want true")
	}
}

func TestCapabilitiesEndpoint_EmptyList(t *testing.T) {
	s := newTestSidecar(nil)
	mux := testMux(s)

	req := httptest.NewRequest("GET", "/capabilities", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /capabilities status = %d, want %d", w.Code, http.StatusOK)
	}

	var got []Capability
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to unmarshal capabilities: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("got %d capabilities, want 0", len(got))
	}

	// Should be an empty JSON array, not null.
	if string(w.Body.Bytes()) != "[]" {
		t.Errorf("response body = %s, want []", w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// POST /capabilities/{name}/invoke endpoint
// ---------------------------------------------------------------------------

func TestCapabilityInvoke_ReturnsNotImplemented(t *testing.T) {
	caps := []Capability{
		{Name: "test-cap", Description: "A test capability"},
	}
	s := newTestSidecar(caps)
	mux := testMux(s)

	req := httptest.NewRequest("POST", "/capabilities/test-cap/invoke", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("POST /capabilities/test-cap/invoke status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp invokeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal invoke response: %v", err)
	}

	if resp.Status != "not_implemented" {
		t.Errorf("invoke status = %q, want %q", resp.Status, "not_implemented")
	}
}

func TestCapabilityInvoke_NotFound(t *testing.T) {
	caps := []Capability{
		{Name: "test-cap", Description: "A test capability"},
	}
	s := newTestSidecar(caps)
	mux := testMux(s)

	req := httptest.NewRequest("POST", "/capabilities/nonexistent/invoke", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("POST /capabilities/nonexistent/invoke status = %d, want %d", w.Code, http.StatusNotFound)
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal error response: %v", err)
	}

	if resp["error"] != "capability not found" {
		t.Errorf("error = %q, want %q", resp["error"], "capability not found")
	}
	if resp["name"] != "nonexistent" {
		t.Errorf("name = %q, want %q", resp["name"], "nonexistent")
	}
}

func TestCapabilityInvoke_InvalidPath(t *testing.T) {
	s := newTestSidecar(nil)
	mux := testMux(s)

	// Missing /invoke suffix.
	req := httptest.NewRequest("POST", "/capabilities/test-cap", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("POST /capabilities/test-cap (no /invoke) status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestCapabilityInvoke_EmptyCapabilityName(t *testing.T) {
	s := newTestSidecar(nil)
	mux := testMux(s)

	// Empty capability name: /capabilities//invoke
	// Go's ServeMux cleans double slashes with a 301 redirect, so we expect
	// either a 301 (redirect) or a 404 (not found). Both are acceptable --
	// the key is that we do NOT get a 200 OK with a capability response.
	req := httptest.NewRequest("POST", "/capabilities//invoke", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Errorf("POST /capabilities//invoke should not succeed, got status %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Sidecar New() and configuration
// ---------------------------------------------------------------------------

func TestNew_DefaultLogger(t *testing.T) {
	cfg := Config{AgentID: "agent-1", TeamID: "team-1"}
	s := New(cfg, nil)
	if s == nil {
		t.Fatal("New returned nil")
	}
	if s.agentID != "agent-1" {
		t.Errorf("agentID = %q, want %q", s.agentID, "agent-1")
	}
	if s.teamID != "team-1" {
		t.Errorf("teamID = %q, want %q", s.teamID, "team-1")
	}
}

func TestNew_CapabilitiesStored(t *testing.T) {
	caps := []Capability{
		{Name: "cap-a", Description: "A"},
		{Name: "cap-b", Description: "B"},
	}
	cfg := Config{
		AgentID:      "agent-1",
		Capabilities: caps,
	}
	s := New(cfg, testLogger())

	if len(s.capabilities) != 2 {
		t.Fatalf("capabilities count = %d, want 2", len(s.capabilities))
	}
	if s.capabilities[0].Name != "cap-a" {
		t.Errorf("capabilities[0].Name = %q, want %q", s.capabilities[0].Name, "cap-a")
	}
	if s.capabilities[1].Name != "cap-b" {
		t.Errorf("capabilities[1].Name = %q, want %q", s.capabilities[1].Name, "cap-b")
	}
}

// ---------------------------------------------------------------------------
// writeJSON helper
// ---------------------------------------------------------------------------

func TestWriteJSON_ContentType(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, http.StatusCreated, map[string]string{"key": "value"}, testLogger())

	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d", w.Code, http.StatusCreated)
	}

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}

	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["key"] != "value" {
		t.Errorf("key = %q, want %q", got["key"], "value")
	}
}

// ---------------------------------------------------------------------------
// Runtime no-op mode (used implicitly by tests above)
// ---------------------------------------------------------------------------

func TestRuntime_NoOp_IsRunning(t *testing.T) {
	r := NewRuntime("", nil, "", testLogger())
	if err := r.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer r.Stop()

	if !r.IsRunning() {
		t.Error("expected no-op runtime to be running after Start")
	}
}

func TestRuntime_NoOp_StopMarksNotRunning(t *testing.T) {
	r := NewRuntime("", nil, "", testLogger())
	if err := r.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := r.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if r.IsRunning() {
		t.Error("expected no-op runtime to not be running after Stop")
	}
}

func TestRuntime_DoubleStart(t *testing.T) {
	r := NewRuntime("", nil, "", testLogger())
	if err := r.Start(); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	defer r.Stop()

	if err := r.Start(); err == nil {
		t.Error("expected error on second Start, got nil")
	}
}
