// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build unit

package capability

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/brmurrell3/hive/internal/testutil"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func unitTestLogger() *slog.Logger {
	t := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return t
}

// echoHandler returns whatever it receives in the inputs.
func echoHandler(_ context.Context, inputs map[string]interface{}) (map[string]interface{}, error) {
	return inputs, nil
}

// errorHandler always returns an error.
func errorHandler(_ context.Context, _ map[string]interface{}) (map[string]interface{}, error) {
	return nil, fmt.Errorf("handler failed intentionally")
}

// panicHandler always panics.
func panicHandler(_ context.Context, _ map[string]interface{}) (map[string]interface{}, error) {
	panic("intentional panic for testing")
}

// slowHandler sleeps for a configurable duration.
func slowHandler(d time.Duration) Handler {
	return func(ctx context.Context, _ map[string]interface{}) (map[string]interface{}, error) {
		select {
		case <-time.After(d):
			return map[string]interface{}{"done": true}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// ---------------------------------------------------------------------------
// NewRouter tests
// ---------------------------------------------------------------------------

func TestNewRouter_InvalidAgentID(t *testing.T) {
	t.Parallel()
	r := NewRouter("agent*bad", nil, unitTestLogger())
	if r == nil {
		t.Fatal("expected non-nil router even with invalid agent ID")
	}
	if r.initErr == nil {
		t.Fatal("expected initErr to be set for invalid agent ID")
	}
}

func TestNewRouter_ValidAgentID(t *testing.T) {
	t.Parallel()
	r := NewRouter("agent-1", nil, unitTestLogger())
	if r == nil {
		t.Fatal("expected non-nil router")
	}
	if r.initErr != nil {
		t.Fatalf("expected no initErr, got: %v", r.initErr)
	}
}

// ---------------------------------------------------------------------------
// RegisterHandler tests
// ---------------------------------------------------------------------------

func TestRegisterHandler_InvalidCapability(t *testing.T) {
	t.Parallel()
	r := NewRouter("agent-1", nil, unitTestLogger())

	err := r.RegisterHandler("bad capability!", echoHandler)
	if err == nil {
		t.Fatal("expected error for invalid capability name")
	}
	if !strings.Contains(err.Error(), "invalid capability name") {
		t.Errorf("expected 'invalid capability name' in error, got: %v", err)
	}
}

func TestRegisterHandler_ValidCapability(t *testing.T) {
	t.Parallel()
	r := NewRouter("agent-1", nil, unitTestLogger())

	err := r.RegisterHandler("echo", echoHandler)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	r.mu.RLock()
	_, exists := r.handlers["echo"]
	r.mu.RUnlock()

	if !exists {
		t.Fatal("expected handler to be registered")
	}
}

// ---------------------------------------------------------------------------
// Start tests
// ---------------------------------------------------------------------------

func TestStart_WithInitError(t *testing.T) {
	t.Parallel()
	r := NewRouter("agent*bad", nil, unitTestLogger())

	err := r.Start()
	if err == nil {
		t.Fatal("expected Start to return initErr")
	}
	if !strings.Contains(err.Error(), "invalid agent ID") {
		t.Errorf("expected 'invalid agent ID' in error, got: %v", err)
	}
}

func TestStart_DoubleStart(t *testing.T) {
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	r := NewRouter("agent-1", nc, unitTestLogger())

	if err := r.Start(); err != nil {
		t.Fatalf("first Start failed: %v", err)
	}
	defer r.Stop()

	err := r.Start()
	if err == nil {
		t.Fatal("expected error on double Start")
	}
	if !strings.Contains(err.Error(), "already started") {
		t.Errorf("expected 'already started' in error, got: %v", err)
	}
}

func TestStart_NoHandlers(t *testing.T) {
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	r := NewRouter("agent-1", nc, unitTestLogger())

	if err := r.Start(); err != nil {
		t.Fatalf("Start with no handlers should succeed, got: %v", err)
	}
	defer r.Stop()

	// Verify the router was started.
	r.mu.RLock()
	started := r.started
	r.mu.RUnlock()

	if !started {
		t.Fatal("expected router to be marked as started")
	}
}

// ---------------------------------------------------------------------------
// Stop tests
// ---------------------------------------------------------------------------

func TestStop_Idempotent(t *testing.T) {
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	r := NewRouter("agent-1", nc, unitTestLogger())
	if err := r.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Calling Stop multiple times should not panic.
	r.Stop()
	r.Stop()
	r.Stop()
}

func TestStop_BeforeStart(t *testing.T) {
	t.Parallel()

	r := NewRouter("agent-1", nil, unitTestLogger())

	// Stop before Start should not panic. stopCh is nil, handled by the nil check.
	r.Stop()
	r.Stop()
}

// ---------------------------------------------------------------------------
// CallLocal tests
// ---------------------------------------------------------------------------

func TestCallLocal_NotFound(t *testing.T) {
	t.Parallel()
	r := NewRouter("agent-1", nil, unitTestLogger())

	resp := r.CallLocal("nonexistent", nil)
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if resp.Status != "error" {
		t.Errorf("expected status=error, got %q", resp.Status)
	}
	if resp.Error == nil {
		t.Fatal("expected error in response")
	}
	if resp.Error.Code != "NOT_FOUND" {
		t.Errorf("expected error code NOT_FOUND, got %q", resp.Error.Code)
	}
	if resp.Error.Retryable {
		t.Error("expected NOT_FOUND to not be retryable")
	}
}

func TestCallLocal_Success(t *testing.T) {
	t.Parallel()
	r := NewRouter("agent-1", nil, unitTestLogger())

	if err := r.RegisterHandler("echo", echoHandler); err != nil {
		t.Fatalf("registering handler: %v", err)
	}

	inputs := map[string]interface{}{"message": "hello"}
	resp := r.CallLocal("echo", inputs)

	if resp.Status != "success" {
		t.Fatalf("expected status=success, got %q", resp.Status)
	}
	if resp.Error != nil {
		t.Fatalf("expected no error, got %+v", resp.Error)
	}
	if resp.Outputs["message"] != "hello" {
		t.Errorf("expected echoed message, got %v", resp.Outputs["message"])
	}
	if resp.DurationMs < 0 {
		t.Errorf("expected non-negative duration_ms, got %d", resp.DurationMs)
	}
	if resp.Capability != "echo" {
		t.Errorf("expected capability=echo, got %q", resp.Capability)
	}
}

func TestCallLocal_HandlerError(t *testing.T) {
	t.Parallel()
	r := NewRouter("agent-1", nil, unitTestLogger())

	if err := r.RegisterHandler("fail", errorHandler); err != nil {
		t.Fatalf("registering handler: %v", err)
	}

	resp := r.CallLocal("fail", nil)
	if resp.Status != "error" {
		t.Fatalf("expected status=error, got %q", resp.Status)
	}
	if resp.Error == nil {
		t.Fatal("expected error in response")
	}
	if resp.Error.Code != "HANDLER_ERROR" {
		t.Errorf("expected error code HANDLER_ERROR, got %q", resp.Error.Code)
	}
	if !strings.Contains(resp.Error.Message, "handler failed intentionally") {
		t.Errorf("expected error message to contain handler error, got %q", resp.Error.Message)
	}
}

func TestCallLocal_HandlerPanic(t *testing.T) {
	t.Parallel()
	r := NewRouter("agent-1", nil, unitTestLogger())

	if err := r.RegisterHandler("panic", panicHandler); err != nil {
		t.Fatalf("registering handler: %v", err)
	}

	resp := r.CallLocal("panic", nil)
	if resp.Status != "error" {
		t.Fatalf("expected status=error, got %q", resp.Status)
	}
	if resp.Error == nil {
		t.Fatal("expected error in response")
	}
	if resp.Error.Code != "HANDLER_ERROR" {
		t.Errorf("expected error code HANDLER_ERROR, got %q", resp.Error.Code)
	}
	if !strings.Contains(resp.Error.Message, "panicked") {
		t.Errorf("expected panic message, got %q", resp.Error.Message)
	}
}

// ---------------------------------------------------------------------------
// executeHandler tests
// ---------------------------------------------------------------------------

func TestExecuteHandler_Success(t *testing.T) {
	t.Parallel()
	r := NewRouter("agent-1", nil, unitTestLogger())

	inputs := map[string]interface{}{"key": "value"}
	outputs, err := r.executeHandler(echoHandler, inputs, 5*time.Second, "test-cap")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if outputs["key"] != "value" {
		t.Errorf("expected key=value, got %v", outputs["key"])
	}
}

func TestExecuteHandler_Timeout(t *testing.T) {
	t.Parallel()
	r := NewRouter("agent-1", nil, unitTestLogger())

	start := time.Now()
	_, err := r.executeHandler(slowHandler(10*time.Second), nil, 100*time.Millisecond, "slow-cap")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected timeout error message, got: %v", err)
	}
	// Should have timed out quickly, not waited for the full handler duration.
	if elapsed > 2*time.Second {
		t.Errorf("timeout took too long: %v", elapsed)
	}
}

func TestExecuteHandler_Panic(t *testing.T) {
	t.Parallel()
	r := NewRouter("agent-1", nil, unitTestLogger())

	_, err := r.executeHandler(panicHandler, nil, 5*time.Second, "panic-cap")
	if err == nil {
		t.Fatal("expected error from panicking handler")
	}
	if !strings.Contains(err.Error(), "panicked") {
		t.Errorf("expected panic recovery message, got: %v", err)
	}
}

func TestExecuteHandler_NilOutputs(t *testing.T) {
	t.Parallel()
	r := NewRouter("agent-1", nil, unitTestLogger())

	nilHandler := func(_ context.Context, _ map[string]interface{}) (map[string]interface{}, error) {
		return nil, nil
	}

	outputs, err := r.executeHandler(nilHandler, nil, 5*time.Second, "nil-cap")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if outputs != nil {
		t.Errorf("expected nil outputs, got: %v", outputs)
	}
}

// ---------------------------------------------------------------------------
// parseTimeout tests
// ---------------------------------------------------------------------------

func TestParseTimeout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected time.Duration
	}{
		{
			name:     "empty string returns default",
			input:    "",
			expected: defaultTimeout,
		},
		{
			name:     "valid duration",
			input:    "5s",
			expected: 5 * time.Second,
		},
		{
			name:     "negative duration returns default",
			input:    "-1s",
			expected: defaultTimeout,
		},
		{
			name:     "zero duration returns default",
			input:    "0s",
			expected: defaultTimeout,
		},
		{
			name:     "over max capped to maxTimeout",
			input:    "10m",
			expected: maxTimeout,
		},
		{
			name:     "exactly max is allowed",
			input:    "5m",
			expected: 5 * time.Minute,
		},
		{
			name:     "invalid string returns default",
			input:    "abc",
			expected: defaultTimeout,
		},
		{
			name:     "sub-second duration",
			input:    "500ms",
			expected: 500 * time.Millisecond,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := parseTimeout(tc.input)
			if got != tc.expected {
				t.Errorf("parseTimeout(%q) = %v, want %v", tc.input, got, tc.expected)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// isDuplicate tests
// ---------------------------------------------------------------------------

func TestIsDuplicate(t *testing.T) {
	t.Parallel()
	r := NewRouter("agent-1", nil, unitTestLogger())

	// Empty ID should never be considered duplicate.
	if r.isDuplicate("") {
		t.Error("empty message ID should not be duplicate")
	}

	// First time seeing an ID should not be duplicate.
	if r.isDuplicate("msg-001") {
		t.Error("first occurrence of msg-001 should not be duplicate")
	}

	// Second time seeing the same ID should be duplicate.
	if !r.isDuplicate("msg-001") {
		t.Error("second occurrence of msg-001 should be duplicate")
	}

	// Different ID should not be duplicate.
	if r.isDuplicate("msg-002") {
		t.Error("first occurrence of msg-002 should not be duplicate")
	}
}

// ---------------------------------------------------------------------------
// sweepDedup tests
// ---------------------------------------------------------------------------

func TestSweepDedup(t *testing.T) {
	t.Parallel()
	r := NewRouter("agent-1", nil, unitTestLogger())

	// Add some entries with timestamps in the past (expired).
	r.dedupMu.Lock()
	expired := time.Now().Add(-3 * dedupTTL)
	r.dedup["old-1"] = dedupEntry{seen: expired}
	r.dedup["old-2"] = dedupEntry{seen: expired}
	// Add a fresh entry that should survive the sweep.
	r.dedup["fresh-1"] = dedupEntry{seen: time.Now()}
	r.dedupMu.Unlock()

	r.sweepDedup()

	r.dedupMu.Lock()
	defer r.dedupMu.Unlock()

	if _, exists := r.dedup["old-1"]; exists {
		t.Error("expired entry old-1 should have been swept")
	}
	if _, exists := r.dedup["old-2"]; exists {
		t.Error("expired entry old-2 should have been swept")
	}
	if _, exists := r.dedup["fresh-1"]; !exists {
		t.Error("fresh entry fresh-1 should still exist")
	}
}

// ---------------------------------------------------------------------------
// DedupHardLimit tests
// ---------------------------------------------------------------------------

func TestDedupHardLimit(t *testing.T) {
	t.Parallel()
	r := NewRouter("agent-1", nil, unitTestLogger())

	// Fill the dedup map to just above the hard limit with non-expired entries.
	r.dedupMu.Lock()
	now := time.Now()
	for i := 0; i < dedupHardLimit+100; i++ {
		// Stagger timestamps slightly so eviction ordering is deterministic.
		r.dedup[fmt.Sprintf("msg-%06d", i)] = dedupEntry{
			seen: now.Add(time.Duration(i) * time.Millisecond),
		}
	}
	r.dedupMu.Unlock()

	// isDuplicate checks and enforces the hard limit on entry.
	// Inserting a new unique ID will trigger the eviction path.
	if r.isDuplicate("trigger-eviction") {
		t.Error("new ID should not be duplicate")
	}

	r.dedupMu.Lock()
	remaining := len(r.dedup)
	r.dedupMu.Unlock()

	// After eviction, the map should be at or below dedupMaxSize + 1
	// (dedupMaxSize after eviction, plus the newly inserted "trigger-eviction" entry).
	if remaining > dedupMaxSize+1 {
		t.Errorf("dedup map should have been evicted to around dedupMaxSize (%d), got %d", dedupMaxSize, remaining)
	}
}

// ---------------------------------------------------------------------------
// RegisterHandler after Start
// ---------------------------------------------------------------------------

func TestRegisterHandler_AfterStart(t *testing.T) {
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	r := NewRouter("agent-1", nc, unitTestLogger())
	if err := r.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer r.Stop()

	// Registering after Start should still succeed (handler is stored)
	// but a warning is logged.
	err := r.RegisterHandler("late-handler", echoHandler)
	if err != nil {
		t.Fatalf("RegisterHandler after Start should succeed, got: %v", err)
	}

	r.mu.RLock()
	_, exists := r.handlers["late-handler"]
	r.mu.RUnlock()

	if !exists {
		t.Fatal("expected late handler to be registered")
	}
}
