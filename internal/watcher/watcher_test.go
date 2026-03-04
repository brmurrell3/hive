// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build unit

package watcher

import (
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// setupAgentDir creates the directory structure:
//
//	<root>/agents/<agentID>/MEMORY.md
//
// and writes the given initial content. Returns the path to MEMORY.md.
func setupAgentDir(t *testing.T, root, agentID, content string) string {
	t.Helper()
	dir := filepath.Join(root, "agents", agentID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating agent dir: %v", err)
	}
	memPath := filepath.Join(dir, "MEMORY.md")
	if err := os.WriteFile(memPath, []byte(content), 0o644); err != nil {
		t.Fatalf("writing MEMORY.md: %v", err)
	}
	return memPath
}

// callbackRecorder records onChange invocations in a thread-safe manner.
type callbackRecorder struct {
	mu    sync.Mutex
	calls []callbackEvent
	ch    chan callbackEvent
}

type callbackEvent struct {
	AgentID string
	Content string
}

func newCallbackRecorder() *callbackRecorder {
	return &callbackRecorder{
		ch: make(chan callbackEvent, 32),
	}
}

func (r *callbackRecorder) onChange(agentID string, content []byte) {
	evt := callbackEvent{AgentID: agentID, Content: string(content)}
	r.mu.Lock()
	r.calls = append(r.calls, evt)
	r.mu.Unlock()
	r.ch <- evt
}

func (r *callbackRecorder) waitForCall(t *testing.T, timeout time.Duration) callbackEvent {
	t.Helper()
	select {
	case evt := <-r.ch:
		return evt
	case <-time.After(timeout):
		t.Fatalf("timeout waiting for onChange callback after %v", timeout)
		return callbackEvent{} // unreachable
	}
}

func (r *callbackRecorder) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// ---------------------------------------------------------------------------
// MEMORY.md change detected
// ---------------------------------------------------------------------------

func TestWatcher_MemoryMdChangeDetected(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	memPath := setupAgentDir(t, root, "agent-a", "initial content")

	rec := newCallbackRecorder()
	w, err := NewWatcher(root, rec.onChange, testLogger())
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}

	if err := w.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if err := w.Stop(); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()

	// Write new content to MEMORY.md.
	newContent := "updated memory content"
	if err := os.WriteFile(memPath, []byte(newContent), 0o644); err != nil {
		t.Fatalf("writing updated MEMORY.md: %v", err)
	}

	// Wait for the callback to fire (debounce is 500ms, so allow up to 5s).
	evt := rec.waitForCall(t, 5*time.Second)

	if evt.AgentID != "agent-a" {
		t.Errorf("agentID = %q, want %q", evt.AgentID, "agent-a")
	}
	if evt.Content != newContent {
		t.Errorf("content = %q, want %q", evt.Content, newContent)
	}
}

// ---------------------------------------------------------------------------
// Debounce: multiple rapid writes result in one callback
// ---------------------------------------------------------------------------

func TestWatcher_Debounce(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	memPath := setupAgentDir(t, root, "agent-debounce", "v0")

	rec := newCallbackRecorder()
	w, err := NewWatcher(root, rec.onChange, testLogger())
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}

	if err := w.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if err := w.Stop(); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()

	// Rapidly write multiple times within the debounce window (500ms).
	for i := 0; i < 5; i++ {
		if err := os.WriteFile(memPath, []byte("rapid write"), 0o644); err != nil {
			t.Fatalf("writing MEMORY.md (iteration %d): %v", i, err)
		}
		time.Sleep(50 * time.Millisecond) // 50ms between writes, well within 500ms debounce
	}

	// Wait for the debounce timer to fire.
	rec.waitForCall(t, 5*time.Second)

	// Wait a bit more to ensure no extra callbacks arrive.
	time.Sleep(1 * time.Second)

	count := rec.callCount()
	if count != 1 {
		t.Errorf("expected exactly 1 debounced callback, got %d", count)
	}
}

// ---------------------------------------------------------------------------
// Multiple agents: separate callbacks
// ---------------------------------------------------------------------------

func TestWatcher_MultipleAgents(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	memPathA := setupAgentDir(t, root, "agent-a", "a-initial")
	memPathB := setupAgentDir(t, root, "agent-b", "b-initial")

	rec := newCallbackRecorder()
	w, err := NewWatcher(root, rec.onChange, testLogger())
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}

	if err := w.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if err := w.Stop(); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()

	// Write to agent-a's MEMORY.md.
	if err := os.WriteFile(memPathA, []byte("a-updated"), 0o644); err != nil {
		t.Fatalf("writing agent-a MEMORY.md: %v", err)
	}

	// Wait slightly longer than debounce to let agent-a's callback fire
	// before triggering agent-b, so we get deterministic ordering.
	time.Sleep(700 * time.Millisecond)

	// Write to agent-b's MEMORY.md.
	if err := os.WriteFile(memPathB, []byte("b-updated"), 0o644); err != nil {
		t.Fatalf("writing agent-b MEMORY.md: %v", err)
	}

	// Collect both callbacks.
	evtA := rec.waitForCall(t, 5*time.Second)
	evtB := rec.waitForCall(t, 5*time.Second)

	// Because we staggered the writes, evtA should be agent-a and evtB agent-b.
	if evtA.AgentID != "agent-a" {
		t.Errorf("first callback agentID = %q, want %q", evtA.AgentID, "agent-a")
	}
	if evtA.Content != "a-updated" {
		t.Errorf("first callback content = %q, want %q", evtA.Content, "a-updated")
	}

	if evtB.AgentID != "agent-b" {
		t.Errorf("second callback agentID = %q, want %q", evtB.AgentID, "agent-b")
	}
	if evtB.Content != "b-updated" {
		t.Errorf("second callback content = %q, want %q", evtB.Content, "b-updated")
	}
}

// ---------------------------------------------------------------------------
// Agent ID extraction
// ---------------------------------------------------------------------------

func TestExtractAgentID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		clusterRoot string
		filePath    string
		wantID      string
		wantErr     bool
	}{
		{
			name:        "standard path",
			clusterRoot: "/var/hive/cluster",
			filePath:    "/var/hive/cluster/agents/my-agent/MEMORY.md",
			wantID:      "my-agent",
			wantErr:     false,
		},
		{
			name:        "agent with dashes and numbers",
			clusterRoot: "/tmp/test",
			filePath:    "/tmp/test/agents/agent-42-prod/MEMORY.md",
			wantID:      "agent-42-prod",
			wantErr:     false,
		},
		{
			name:        "file not under agents directory",
			clusterRoot: "/var/hive/cluster",
			filePath:    "/var/hive/cluster/config/MEMORY.md",
			wantID:      "",
			wantErr:     true,
		},
		{
			name:        "path with trailing slash normalization",
			clusterRoot: "/var/hive/cluster/",
			filePath:    "/var/hive/cluster/agents/normalized-agent/MEMORY.md",
			wantID:      "normalized-agent",
			wantErr:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotID, err := extractAgentID(tt.clusterRoot, tt.filePath)
			if (err != nil) != tt.wantErr {
				t.Errorf("extractAgentID() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if gotID != tt.wantID {
				t.Errorf("extractAgentID() = %q, want %q", gotID, tt.wantID)
			}
		})
	}
}
