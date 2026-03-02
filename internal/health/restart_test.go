// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build unit

package health

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/types"
)

// ---------------------------------------------------------------------------
// Mock VMManager
// ---------------------------------------------------------------------------

type mockVMManager struct {
	mu       sync.Mutex
	stopped  []string
	started  []*types.AgentManifest
	stopErr  error
	startErr error
}

func (m *mockVMManager) StopAgent(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopped = append(m.stopped, id)
	return m.stopErr
}

func (m *mockVMManager) StartAgent(_ context.Context, agent *types.AgentManifest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.started = append(m.started, agent)
	return m.startErr
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func testLoggerUnit() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func testStoreUnit(t *testing.T) *state.Store {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	store, err := state.NewStore(path, testLoggerUnit())
	if err != nil {
		t.Fatalf("creating test store: %v", err)
	}
	return store
}

func testManifest(agentID string) *types.AgentManifest {
	return &types.AgentManifest{
		APIVersion: "hive/v1alpha1",
		Kind:       "Agent",
		Metadata: types.AgentMetadata{
			ID:   agentID,
			Team: "test-team",
		},
		Spec: types.AgentSpec{
			Runtime: types.AgentRuntime{
				Type: "firecracker",
			},
		},
	}
}

// ---------------------------------------------------------------------------
// on-failure policy restarts on crash (agent in RUNNING state)
// ---------------------------------------------------------------------------

func TestRestartManager_OnFailurePolicy_RestartsOnCrash(t *testing.T) {
	store := testStoreUnit(t)
	vmMgr := &mockVMManager{}
	rm := NewRestartManager(store, vmMgr, testLoggerUnit())

	agentID := "agent-crash"
	manifest := testManifest(agentID)

	// Set agent to RUNNING state (simulating a healthy agent that crashed).
	agent := &state.AgentState{
		ID:             agentID,
		Team:           "test-team",
		Status:         state.AgentStatusRunning,
		RestartCount:   0,
		LastTransition: time.Now(),
	}
	if err := store.SetAgent(agent); err != nil {
		t.Fatalf("setting initial agent state: %v", err)
	}

	rm.SetConfig(agentID, RestartConfig{
		Policy:      "on-failure",
		MaxRestarts: 5,
		Backoff:     0, // no backoff for tests
		Manifest:    manifest,
	})

	if err := rm.HandleUnhealthy(agentID); err != nil {
		t.Fatalf("HandleUnhealthy: %v", err)
	}

	// Verify StopAgent was called.
	vmMgr.mu.Lock()
	stoppedCount := len(vmMgr.stopped)
	startedCount := len(vmMgr.started)
	vmMgr.mu.Unlock()

	if stoppedCount != 1 {
		t.Errorf("expected 1 StopAgent call, got %d", stoppedCount)
	}
	if startedCount != 1 {
		t.Errorf("expected 1 StartAgent call, got %d", startedCount)
	}

	// Verify restart count was incremented.
	got := store.GetAgent(agentID)
	if got == nil {
		t.Fatal("agent not found in store")
	}
	if got.RestartCount != 1 {
		t.Errorf("RestartCount = %d, want 1", got.RestartCount)
	}
}

// ---------------------------------------------------------------------------
// on-failure policy does NOT restart when agent is in non-restartable state
// ---------------------------------------------------------------------------

func TestRestartManager_OnFailurePolicy_SkipsNonRestartableState(t *testing.T) {
	store := testStoreUnit(t)
	vmMgr := &mockVMManager{}
	rm := NewRestartManager(store, vmMgr, testLoggerUnit())

	agentID := "agent-stopped"
	manifest := testManifest(agentID)

	// Set agent to STOPPED state (not RUNNING or FAILED).
	agent := &state.AgentState{
		ID:             agentID,
		Team:           "test-team",
		Status:         state.AgentStatusStopped,
		RestartCount:   0,
		LastTransition: time.Now(),
	}
	if err := store.SetAgent(agent); err != nil {
		t.Fatalf("setting initial agent state: %v", err)
	}

	rm.SetConfig(agentID, RestartConfig{
		Policy:      "on-failure",
		MaxRestarts: 5,
		Backoff:     0,
		Manifest:    manifest,
	})

	if err := rm.HandleUnhealthy(agentID); err != nil {
		t.Fatalf("HandleUnhealthy: %v", err)
	}

	// Verify no stop/start calls were made.
	vmMgr.mu.Lock()
	stoppedCount := len(vmMgr.stopped)
	startedCount := len(vmMgr.started)
	vmMgr.mu.Unlock()

	if stoppedCount != 0 {
		t.Errorf("expected 0 StopAgent calls, got %d", stoppedCount)
	}
	if startedCount != 0 {
		t.Errorf("expected 0 StartAgent calls, got %d", startedCount)
	}
}

// ---------------------------------------------------------------------------
// always policy restarts regardless of state
// ---------------------------------------------------------------------------

func TestRestartManager_AlwaysPolicy_Restarts(t *testing.T) {
	store := testStoreUnit(t)
	vmMgr := &mockVMManager{}
	rm := NewRestartManager(store, vmMgr, testLoggerUnit())

	agentID := "agent-always"
	manifest := testManifest(agentID)

	// Set agent to STOPPED state - "always" should still restart.
	agent := &state.AgentState{
		ID:             agentID,
		Team:           "test-team",
		Status:         state.AgentStatusStopped,
		RestartCount:   0,
		LastTransition: time.Now(),
	}
	if err := store.SetAgent(agent); err != nil {
		t.Fatalf("setting initial agent state: %v", err)
	}

	rm.SetConfig(agentID, RestartConfig{
		Policy:      "always",
		MaxRestarts: 10,
		Backoff:     0,
		Manifest:    manifest,
	})

	if err := rm.HandleUnhealthy(agentID); err != nil {
		t.Fatalf("HandleUnhealthy: %v", err)
	}

	vmMgr.mu.Lock()
	stoppedCount := len(vmMgr.stopped)
	startedCount := len(vmMgr.started)
	vmMgr.mu.Unlock()

	if stoppedCount != 1 {
		t.Errorf("expected 1 StopAgent call, got %d", stoppedCount)
	}
	if startedCount != 1 {
		t.Errorf("expected 1 StartAgent call, got %d", startedCount)
	}

	got := store.GetAgent(agentID)
	if got == nil {
		t.Fatal("agent not found in store")
	}
	if got.RestartCount != 1 {
		t.Errorf("RestartCount = %d, want 1", got.RestartCount)
	}
}

// ---------------------------------------------------------------------------
// never policy does not restart
// ---------------------------------------------------------------------------

func TestRestartManager_NeverPolicy_DoesNotRestart(t *testing.T) {
	store := testStoreUnit(t)
	vmMgr := &mockVMManager{}
	rm := NewRestartManager(store, vmMgr, testLoggerUnit())

	agentID := "agent-never"
	manifest := testManifest(agentID)

	agent := &state.AgentState{
		ID:             agentID,
		Team:           "test-team",
		Status:         state.AgentStatusRunning,
		RestartCount:   0,
		LastTransition: time.Now(),
	}
	if err := store.SetAgent(agent); err != nil {
		t.Fatalf("setting initial agent state: %v", err)
	}

	rm.SetConfig(agentID, RestartConfig{
		Policy:      "never",
		MaxRestarts: 5,
		Backoff:     0,
		Manifest:    manifest,
	})

	if err := rm.HandleUnhealthy(agentID); err != nil {
		t.Fatalf("HandleUnhealthy: %v", err)
	}

	vmMgr.mu.Lock()
	stoppedCount := len(vmMgr.stopped)
	startedCount := len(vmMgr.started)
	vmMgr.mu.Unlock()

	if stoppedCount != 0 {
		t.Errorf("expected 0 StopAgent calls, got %d", stoppedCount)
	}
	if startedCount != 0 {
		t.Errorf("expected 0 StartAgent calls, got %d", startedCount)
	}

	// Agent state should remain unchanged.
	got := store.GetAgent(agentID)
	if got == nil {
		t.Fatal("agent not found in store")
	}
	if got.RestartCount != 0 {
		t.Errorf("RestartCount = %d, want 0", got.RestartCount)
	}
	if got.Status != state.AgentStatusRunning {
		t.Errorf("Status = %q, want %q", got.Status, state.AgentStatusRunning)
	}
}

// ---------------------------------------------------------------------------
// maxRestarts exceeded transitions agent to FAILED
// ---------------------------------------------------------------------------

func TestRestartManager_MaxRestartsExceeded_TransitionsToFailed(t *testing.T) {
	store := testStoreUnit(t)
	vmMgr := &mockVMManager{}
	rm := NewRestartManager(store, vmMgr, testLoggerUnit())

	agentID := "agent-maxed"
	manifest := testManifest(agentID)

	// Set agent already at the maxRestarts limit.
	agent := &state.AgentState{
		ID:             agentID,
		Team:           "test-team",
		Status:         state.AgentStatusRunning,
		RestartCount:   3, // already at max
		LastTransition: time.Now(),
	}
	if err := store.SetAgent(agent); err != nil {
		t.Fatalf("setting initial agent state: %v", err)
	}

	rm.SetConfig(agentID, RestartConfig{
		Policy:      "always",
		MaxRestarts: 3,
		Backoff:     0,
		Manifest:    manifest,
	})

	if err := rm.HandleUnhealthy(agentID); err != nil {
		t.Fatalf("HandleUnhealthy: %v", err)
	}

	// Verify no stop/start calls were made (agent transitions directly to FAILED).
	vmMgr.mu.Lock()
	stoppedCount := len(vmMgr.stopped)
	startedCount := len(vmMgr.started)
	vmMgr.mu.Unlock()

	if stoppedCount != 0 {
		t.Errorf("expected 0 StopAgent calls when max exceeded, got %d", stoppedCount)
	}
	if startedCount != 0 {
		t.Errorf("expected 0 StartAgent calls when max exceeded, got %d", startedCount)
	}

	got := store.GetAgent(agentID)
	if got == nil {
		t.Fatal("agent not found in store")
	}
	if got.Status != state.AgentStatusFailed {
		t.Errorf("Status = %q, want %q", got.Status, state.AgentStatusFailed)
	}
	if got.Error == "" {
		t.Error("expected error message when transitioning to FAILED")
	}
	// RestartCount should remain unchanged (no new restart was attempted).
	if got.RestartCount != 3 {
		t.Errorf("RestartCount = %d, want 3 (unchanged)", got.RestartCount)
	}
}

// ---------------------------------------------------------------------------
// Restart count increments on each restart
// ---------------------------------------------------------------------------

func TestRestartManager_RestartCountIncrements(t *testing.T) {
	store := testStoreUnit(t)
	vmMgr := &mockVMManager{}
	rm := NewRestartManager(store, vmMgr, testLoggerUnit())

	agentID := "agent-counting"
	manifest := testManifest(agentID)

	agent := &state.AgentState{
		ID:             agentID,
		Team:           "test-team",
		Status:         state.AgentStatusRunning,
		RestartCount:   0,
		LastTransition: time.Now(),
	}
	if err := store.SetAgent(agent); err != nil {
		t.Fatalf("setting initial agent state: %v", err)
	}

	rm.SetConfig(agentID, RestartConfig{
		Policy:      "always",
		MaxRestarts: 10,
		Backoff:     0,
		Manifest:    manifest,
	})

	// Trigger 3 consecutive restarts. After each HandleUnhealthy the agent
	// state is persisted with the updated RestartCount, so subsequent calls
	// will see the incremented value.
	for i := 1; i <= 3; i++ {
		if err := rm.HandleUnhealthy(agentID); err != nil {
			t.Fatalf("HandleUnhealthy (iteration %d): %v", i, err)
		}

		got := store.GetAgent(agentID)
		if got == nil {
			t.Fatalf("agent not found in store after restart %d", i)
		}
		if got.RestartCount != i {
			t.Errorf("after restart %d: RestartCount = %d, want %d", i, got.RestartCount, i)
		}
	}

	// Verify the total number of VM operations.
	vmMgr.mu.Lock()
	stoppedCount := len(vmMgr.stopped)
	startedCount := len(vmMgr.started)
	vmMgr.mu.Unlock()

	if stoppedCount != 3 {
		t.Errorf("expected 3 StopAgent calls, got %d", stoppedCount)
	}
	if startedCount != 3 {
		t.Errorf("expected 3 StartAgent calls, got %d", startedCount)
	}
}
