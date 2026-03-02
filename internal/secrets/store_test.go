// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build unit

package secrets

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/brmurrell3/hive/internal/testutil"
	"github.com/nats-io/nats.go"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// writeSecretsFile creates a YAML secrets file in a temp directory and returns
// its path. The caller is responsible for cleanup (via t.TempDir).
func writeSecretsFile(t *testing.T, dir, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, "secrets.yaml")
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("writing secrets file: %v", err)
	}
	return path
}

// sendRequest publishes a request to hive.secrets.request and returns the
// decoded secretResponse. It fails the test if anything goes wrong or the
// request times out.
func sendRequest(t *testing.T, nc *nats.Conn, req secretRequest) secretResponse {
	t.Helper()

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshaling request: %v", err)
	}

	msg, err := nc.Request("hive.secrets.request", data, 3*time.Second)
	if err != nil {
		t.Fatalf("sending NATS request: %v", err)
	}

	var resp secretResponse
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		t.Fatalf("unmarshaling response: %v", err)
	}
	return resp
}

// ---------------------------------------------------------------------------
// NewStore tests
// ---------------------------------------------------------------------------

func TestNewStore_EmptyPath(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	store, err := NewStore("", nc, testLogger())
	if err != nil {
		t.Fatalf("NewStore with empty path: %v", err)
	}
	if store == nil {
		t.Fatal("expected non-nil store, got nil")
	}

	// An empty-path store should have no secrets.
	if _, ok := store.Get("anything"); ok {
		t.Error("expected no secrets in empty store, but Get returned ok=true")
	}
}

func TestNewStore_ValidFile(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	dir := t.TempDir()
	path := writeSecretsFile(t, dir, `secrets:
  db_password: "s3cr3t"
  api_key: "abc123"
`, 0o600)

	store, err := NewStore(path, nc, testLogger())
	if err != nil {
		t.Fatalf("NewStore with valid file: %v", err)
	}
	if store == nil {
		t.Fatal("expected non-nil store, got nil")
	}

	// Verify both secrets were loaded.
	val, ok := store.Get("db_password")
	if !ok {
		t.Error("expected db_password to be present")
	}
	if val != "s3cr3t" {
		t.Errorf("db_password = %q, want %q", val, "s3cr3t")
	}

	val, ok = store.Get("api_key")
	if !ok {
		t.Error("expected api_key to be present")
	}
	if val != "abc123" {
		t.Errorf("api_key = %q, want %q", val, "abc123")
	}
}

func TestNewStore_InvalidFile_NonExistent(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	_, err := NewStore("/nonexistent/path/secrets.yaml", nc, testLogger())
	if err == nil {
		t.Error("expected error for non-existent file, got nil")
	}
}

func TestNewStore_InvalidFile_BadYAML(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	dir := t.TempDir()
	path := writeSecretsFile(t, dir, `not: valid: yaml: [[[`, 0o600)

	_, err := NewStore(path, nc, testLogger())
	if err == nil {
		t.Error("expected error for invalid YAML, got nil")
	}
}

func TestNewStore_WorldReadablePermissionsWarning(t *testing.T) {
	t.Parallel()
	// This test verifies that NewStore still succeeds (returns no error) when
	// the secrets file has overly permissive permissions — it only logs a
	// warning rather than returning an error.
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	dir := t.TempDir()
	// Write a file with world-readable permissions (0644).
	path := writeSecretsFile(t, dir, `secrets:
  key: "value"
`, 0o644)

	store, err := NewStore(path, nc, testLogger())
	if err != nil {
		t.Fatalf("NewStore should succeed (only warn) for world-readable file, got error: %v", err)
	}
	if store == nil {
		t.Fatal("expected non-nil store")
	}

	// Secret should still be accessible.
	val, ok := store.Get("key")
	if !ok {
		t.Error("expected secret 'key' to be present")
	}
	if val != "value" {
		t.Errorf("key = %q, want %q", val, "value")
	}
}

// ---------------------------------------------------------------------------
// Get tests
// ---------------------------------------------------------------------------

func TestGet_ExistingSecret(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	dir := t.TempDir()
	path := writeSecretsFile(t, dir, `secrets:
  token: "my-token-value"
`, 0o600)

	store, err := NewStore(path, nc, testLogger())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	val, ok := store.Get("token")
	if !ok {
		t.Fatal("expected Get to return ok=true for existing secret")
	}
	if val != "my-token-value" {
		t.Errorf("Get(token) = %q, want %q", val, "my-token-value")
	}
}

func TestGet_NonExistingSecret(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	dir := t.TempDir()
	path := writeSecretsFile(t, dir, `secrets:
  existing: "value"
`, 0o600)

	store, err := NewStore(path, nc, testLogger())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	val, ok := store.Get("missing_secret")
	if ok {
		t.Error("expected Get to return ok=false for non-existing secret")
	}
	if val != "" {
		t.Errorf("Get(missing_secret) = %q, want empty string", val)
	}
}

func TestGet_EmptyStore(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	// Empty path -> no secrets loaded.
	store, err := NewStore("", nc, testLogger())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	_, ok := store.Get("anything")
	if ok {
		t.Error("expected ok=false from empty store, got true")
	}
}

// ---------------------------------------------------------------------------
// SetAllowedSecrets tests
// ---------------------------------------------------------------------------

func TestSetAllowedSecrets_FiltersByAgent(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	dir := t.TempDir()
	path := writeSecretsFile(t, dir, `secrets:
  db_password: "dbpass"
  api_key: "apikey"
  internal_key: "internal"
`, 0o600)

	store, err := NewStore(path, nc, testLogger())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	if err := store.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer store.Stop()

	// Configure per-agent access:
	// agent-a can access db_password only.
	// agent-b can access api_key and internal_key.
	store.SetAllowedSecrets(map[string]map[string]bool{
		"agent-a": {"db_password": true},
		"agent-b": {"api_key": true, "internal_key": true},
	})

	clientNC := testutil.NATSConnect(t, srv)

	// agent-a requests all three secrets; should only receive db_password.
	resp := sendRequest(t, clientNC, secretRequest{
		AgentID: "agent-a",
		Names:   []string{"db_password", "api_key", "internal_key"},
	})
	if resp.Error != "" {
		t.Fatalf("unexpected error for agent-a: %q", resp.Error)
	}
	if _, ok := resp.Secrets["db_password"]; !ok {
		t.Error("agent-a should have access to db_password")
	}
	if _, ok := resp.Secrets["api_key"]; ok {
		t.Error("agent-a should NOT have access to api_key")
	}
	if _, ok := resp.Secrets["internal_key"]; ok {
		t.Error("agent-a should NOT have access to internal_key")
	}

	// agent-b requests all three; should receive api_key and internal_key only.
	resp = sendRequest(t, clientNC, secretRequest{
		AgentID: "agent-b",
		Names:   []string{"db_password", "api_key", "internal_key"},
	})
	if resp.Error != "" {
		t.Fatalf("unexpected error for agent-b: %q", resp.Error)
	}
	if _, ok := resp.Secrets["db_password"]; ok {
		t.Error("agent-b should NOT have access to db_password")
	}
	if _, ok := resp.Secrets["api_key"]; !ok {
		t.Error("agent-b should have access to api_key")
	}
	if _, ok := resp.Secrets["internal_key"]; !ok {
		t.Error("agent-b should have access to internal_key")
	}
}

func TestSetAllowedSecrets_UnknownAgentGetsEmpty(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	dir := t.TempDir()
	path := writeSecretsFile(t, dir, `secrets:
  secret1: "value1"
`, 0o600)

	store, err := NewStore(path, nc, testLogger())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	if err := store.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer store.Stop()

	// Only agent-known is in the allowlist.
	store.SetAllowedSecrets(map[string]map[string]bool{
		"agent-known": {"secret1": true},
	})

	clientNC := testutil.NATSConnect(t, srv)

	// agent-unknown is not in the allowlist and should receive an empty secrets map.
	resp := sendRequest(t, clientNC, secretRequest{
		AgentID: "agent-unknown",
		Names:   []string{"secret1"},
	})
	if resp.Error != "" {
		t.Fatalf("unexpected error for agent-unknown: %q", resp.Error)
	}
	if len(resp.Secrets) != 0 {
		t.Errorf("agent-unknown should get empty secrets, got %v", resp.Secrets)
	}
}

func TestSetAllowedSecrets_NilRestoresLegacyMode(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	dir := t.TempDir()
	path := writeSecretsFile(t, dir, `secrets:
  key: "val"
`, 0o600)

	store, err := NewStore(path, nc, testLogger())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	if err := store.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer store.Stop()

	// First restrict access.
	store.SetAllowedSecrets(map[string]map[string]bool{
		"agent-a": {"key": true},
	})

	// Then reset to nil (legacy: all agents get all secrets).
	store.SetAllowedSecrets(nil)

	clientNC := testutil.NATSConnect(t, srv)

	// Any agent should now be able to access the secret.
	resp := sendRequest(t, clientNC, secretRequest{
		AgentID: "any-agent-whatsoever",
		Names:   []string{"key"},
	})
	if resp.Error != "" {
		t.Fatalf("unexpected error in legacy mode: %q", resp.Error)
	}
	val, ok := resp.Secrets["key"]
	if !ok {
		t.Error("expected 'key' in response in legacy mode")
	}
	if val != "val" {
		t.Errorf("key = %q, want %q", val, "val")
	}
}

// ---------------------------------------------------------------------------
// Start / Stop tests
// ---------------------------------------------------------------------------

func TestStart_SubscribesToNATS(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	store, err := NewStore("", nc, testLogger())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	if err := store.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer store.Stop()

	// Verify the subscription is live by sending a simple valid request.
	clientNC := testutil.NATSConnect(t, srv)
	resp := sendRequest(t, clientNC, secretRequest{
		AgentID: "any-agent",
		Names:   []string{},
	})

	// With an empty store and no secrets requested, we expect an empty (non-error) response.
	if resp.Error != "" {
		t.Errorf("unexpected error: %q", resp.Error)
	}
	if resp.Secrets == nil {
		t.Error("expected non-nil Secrets map in response")
	}
}

func TestStop_UnsubscribesFromNATS(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	store, err := NewStore("", nc, testLogger())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	if err := store.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	store.Stop()

	// After Stop, the subscription should be gone — requests should time out.
	clientNC := testutil.NATSConnect(t, srv)
	data, _ := json.Marshal(secretRequest{AgentID: "agent-x", Names: []string{}})
	_, err = clientNC.Request("hive.secrets.request", data, 300*time.Millisecond)
	if err == nil {
		t.Error("expected timeout after Stop, but got a response")
	}
}

func TestStop_IdempotentWhenNotStarted(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	store, err := NewStore("", nc, testLogger())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	// Stop without Start should not panic.
	store.Stop()
	store.Stop() // second call is also safe.
}

// ---------------------------------------------------------------------------
// NATS subscription handler tests
// ---------------------------------------------------------------------------

func TestHandler_ValidRequest(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	dir := t.TempDir()
	path := writeSecretsFile(t, dir, `secrets:
  password: "hunter2"
  token: "tok123"
`, 0o600)

	store, err := NewStore(path, nc, testLogger())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer store.Stop()

	clientNC := testutil.NATSConnect(t, srv)

	resp := sendRequest(t, clientNC, secretRequest{
		AgentID: "agent-1",
		Names:   []string{"password", "token"},
	})

	if resp.Error != "" {
		t.Fatalf("unexpected error: %q", resp.Error)
	}
	if resp.Secrets["password"] != "hunter2" {
		t.Errorf("password = %q, want %q", resp.Secrets["password"], "hunter2")
	}
	if resp.Secrets["token"] != "tok123" {
		t.Errorf("token = %q, want %q", resp.Secrets["token"], "tok123")
	}
}

func TestHandler_EmptyAgentID(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	store, err := NewStore("", nc, testLogger())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer store.Stop()

	clientNC := testutil.NATSConnect(t, srv)

	resp := sendRequest(t, clientNC, secretRequest{
		AgentID: "", // empty agent ID
		Names:   []string{"some-secret"},
	})

	if resp.Error == "" {
		t.Error("expected error for empty agent_id, got empty error string")
	}
	if resp.Error != "agent_id is required" {
		t.Errorf("error = %q, want %q", resp.Error, "agent_id is required")
	}
}

func TestHandler_AgentNotInAllowlist(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	dir := t.TempDir()
	path := writeSecretsFile(t, dir, `secrets:
  secret: "classified"
`, 0o600)

	store, err := NewStore(path, nc, testLogger())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer store.Stop()

	// Configure allowlist with only agent-allowed.
	store.SetAllowedSecrets(map[string]map[string]bool{
		"agent-allowed": {"secret": true},
	})

	clientNC := testutil.NATSConnect(t, srv)

	// agent-denied is not in the allowlist.
	resp := sendRequest(t, clientNC, secretRequest{
		AgentID: "agent-denied",
		Names:   []string{"secret"},
	})

	// Should return an empty secrets map (no error, just empty).
	if resp.Error != "" {
		t.Errorf("unexpected error for denied agent: %q", resp.Error)
	}
	if len(resp.Secrets) != 0 {
		t.Errorf("denied agent should receive empty secrets, got %v", resp.Secrets)
	}
}

func TestHandler_LegacyMode_NilAllowedSecrets(t *testing.T) {
	t.Parallel()
	// When allowedSecrets is nil (the zero value / legacy mode), all agents
	// should receive all requested secrets without filtering.
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	dir := t.TempDir()
	path := writeSecretsFile(t, dir, `secrets:
  alpha: "a"
  beta: "b"
`, 0o600)

	store, err := NewStore(path, nc, testLogger())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer store.Stop()

	// allowedSecrets is nil by default — no SetAllowedSecrets call needed.

	clientNC := testutil.NATSConnect(t, srv)

	// Any arbitrary agent should receive requested secrets.
	for _, agentID := range []string{"agent-x", "agent-y", "completely-unknown-agent"} {
		resp := sendRequest(t, clientNC, secretRequest{
			AgentID: agentID,
			Names:   []string{"alpha", "beta"},
		})
		if resp.Error != "" {
			t.Errorf("agent %q: unexpected error in legacy mode: %q", agentID, resp.Error)
			continue
		}
		if resp.Secrets["alpha"] != "a" {
			t.Errorf("agent %q: alpha = %q, want %q", agentID, resp.Secrets["alpha"], "a")
		}
		if resp.Secrets["beta"] != "b" {
			t.Errorf("agent %q: beta = %q, want %q", agentID, resp.Secrets["beta"], "b")
		}
	}
}

func TestHandler_InvalidJSONRequest(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	store, err := NewStore("", nc, testLogger())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer store.Stop()

	clientNC := testutil.NATSConnect(t, srv)

	// Send malformed JSON directly.
	msg, err := clientNC.Request("hive.secrets.request", []byte(`{not valid json`), 3*time.Second)
	if err != nil {
		t.Fatalf("sending invalid JSON request: %v", err)
	}

	var resp secretResponse
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		t.Fatalf("unmarshaling response: %v", err)
	}

	if resp.Error == "" {
		t.Error("expected error for invalid JSON, got empty error string")
	}
	if resp.Error != "invalid request" {
		t.Errorf("error = %q, want %q", resp.Error, "invalid request")
	}
}

func TestHandler_RequestedSecretNotInStore(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	dir := t.TempDir()
	path := writeSecretsFile(t, dir, `secrets:
  exists: "present"
`, 0o600)

	store, err := NewStore(path, nc, testLogger())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer store.Stop()

	clientNC := testutil.NATSConnect(t, srv)

	// Request a mix of existing and non-existing secrets; only existing are returned.
	resp := sendRequest(t, clientNC, secretRequest{
		AgentID: "agent-1",
		Names:   []string{"exists", "does_not_exist"},
	})

	if resp.Error != "" {
		t.Fatalf("unexpected error: %q", resp.Error)
	}
	if resp.Secrets["exists"] != "present" {
		t.Errorf("exists = %q, want %q", resp.Secrets["exists"], "present")
	}
	if _, ok := resp.Secrets["does_not_exist"]; ok {
		t.Error("does_not_exist should not be in the response")
	}
}

func TestHandler_AllowlistFiltersDeniedSecretNames(t *testing.T) {
	t.Parallel()
	// An agent may be in the allowlist but only for specific secrets.
	// Requesting a secret it is not allowed should silently omit it.
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	dir := t.TempDir()
	path := writeSecretsFile(t, dir, `secrets:
  allowed_secret: "yes"
  denied_secret: "no"
`, 0o600)

	store, err := NewStore(path, nc, testLogger())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer store.Stop()

	store.SetAllowedSecrets(map[string]map[string]bool{
		"agent-limited": {"allowed_secret": true},
		// denied_secret is NOT in agent-limited's allowlist.
	})

	clientNC := testutil.NATSConnect(t, srv)

	resp := sendRequest(t, clientNC, secretRequest{
		AgentID: "agent-limited",
		Names:   []string{"allowed_secret", "denied_secret"},
	})

	if resp.Error != "" {
		t.Fatalf("unexpected error: %q", resp.Error)
	}
	if resp.Secrets["allowed_secret"] != "yes" {
		t.Errorf("allowed_secret = %q, want %q", resp.Secrets["allowed_secret"], "yes")
	}
	if _, ok := resp.Secrets["denied_secret"]; ok {
		t.Error("denied_secret should be filtered out for agent-limited")
	}
}

func TestHandler_EmptyNamesReturnsEmptySecrets(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	dir := t.TempDir()
	path := writeSecretsFile(t, dir, `secrets:
  key: "val"
`, 0o600)

	store, err := NewStore(path, nc, testLogger())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer store.Stop()

	clientNC := testutil.NATSConnect(t, srv)

	resp := sendRequest(t, clientNC, secretRequest{
		AgentID: "agent-1",
		Names:   []string{},
	})

	if resp.Error != "" {
		t.Fatalf("unexpected error: %q", resp.Error)
	}
	if len(resp.Secrets) != 0 {
		t.Errorf("expected empty secrets for empty names, got %v", resp.Secrets)
	}
}
