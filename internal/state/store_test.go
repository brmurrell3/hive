//go:build unit

package state

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/brmurrell3/hive/internal/types"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func testStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store, err := NewStore(path, logger)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store, path
}

func testAgentState(id, team string, status AgentStatus) *AgentState {
	return &AgentState{
		ID:             id,
		Team:           team,
		Status:         status,
		LastTransition: time.Now(),
	}
}

// ---------------------------------------------------------------------------
// State machine transitions
// ---------------------------------------------------------------------------

func TestValidateTransition(t *testing.T) {
	tests := []struct {
		name    string
		from    AgentStatus
		to      AgentStatus
		wantErr bool
	}{
		// Valid transitions.
		{"PENDING -> CREATING", AgentStatusPending, AgentStatusCreating, false},
		{"CREATING -> STARTING", AgentStatusCreating, AgentStatusStarting, false},
		{"CREATING -> FAILED", AgentStatusCreating, AgentStatusFailed, false},
		{"STARTING -> RUNNING", AgentStatusStarting, AgentStatusRunning, false},
		{"STARTING -> FAILED", AgentStatusStarting, AgentStatusFailed, false},
		{"RUNNING -> STOPPING", AgentStatusRunning, AgentStatusStopping, false},
		{"RUNNING -> FAILED", AgentStatusRunning, AgentStatusFailed, false},
		{"STOPPING -> STOPPED", AgentStatusStopping, AgentStatusStopped, false},
		{"STOPPING -> FAILED", AgentStatusStopping, AgentStatusFailed, false},
		{"STOPPED -> CREATING (restart)", AgentStatusStopped, AgentStatusCreating, false},
		{"STOPPED -> PENDING (restart reset)", AgentStatusStopped, AgentStatusPending, false},
		{"FAILED -> CREATING (manual restart)", AgentStatusFailed, AgentStatusCreating, false},
		{"FAILED -> STOPPED (restart reset)", AgentStatusFailed, AgentStatusStopped, false},
		{"FAILED -> PENDING (restart reset)", AgentStatusFailed, AgentStatusPending, false},
		{"PENDING -> FAILED (early validation)", AgentStatusPending, AgentStatusFailed, false},

		// Invalid transitions.
		{"PENDING -> RUNNING (skip)", AgentStatusPending, AgentStatusRunning, true},
		{"PENDING -> STARTING (skip)", AgentStatusPending, AgentStatusStarting, true},
		{"PENDING -> STOPPED", AgentStatusPending, AgentStatusStopped, true},
		{"RUNNING -> CREATING (no direct restart)", AgentStatusRunning, AgentStatusCreating, true},
		{"RUNNING -> STOPPED (skip STOPPING)", AgentStatusRunning, AgentStatusStopped, true},
		{"RUNNING -> STARTING (backwards)", AgentStatusRunning, AgentStatusStarting, true},
		{"STOPPED -> RUNNING (skip)", AgentStatusStopped, AgentStatusRunning, true},
		{"STOPPED -> STARTING (skip)", AgentStatusStopped, AgentStatusStarting, true},
		{"FAILED -> RUNNING (skip)", AgentStatusFailed, AgentStatusRunning, true},
		{"CREATING -> RUNNING (skip)", AgentStatusCreating, AgentStatusRunning, true},
		{"STOPPING -> CREATING", AgentStatusStopping, AgentStatusCreating, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateTransition(tt.from, tt.to)
			if tt.wantErr && err == nil {
				t.Errorf("expected error for %s -> %s, got nil", tt.from, tt.to)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error for %s -> %s: %v", tt.from, tt.to, err)
			}
		})
	}
}

func TestValidateTransition_UnknownStatus(t *testing.T) {
	err := ValidateTransition(AgentStatus("UNKNOWN"), AgentStatusRunning)
	if err == nil {
		t.Fatal("expected error for unknown status, got nil")
	}
}

// ---------------------------------------------------------------------------
// Agent state round-trip through SQLite
// ---------------------------------------------------------------------------

func TestSetAgent_RoundTrip(t *testing.T) {
	store, _ := testStore(t)

	now := time.Now().Truncate(time.Millisecond)
	agent := &AgentState{
		ID:             "agent-1",
		Team:           "backend",
		Status:         AgentStatusRunning,
		VMPID:          12345,
		VMCID:          42,
		VMSocketPath:   "/tmp/agent-1.sock",
		RootfsCopyPath: "/tmp/rootfs-copy.ext4",
		RestartCount:   3,
		LastTransition: now,
		StartedAt:      now.Add(-5 * time.Minute),
		Error:          "",
	}

	if err := store.SetAgent(agent); err != nil {
		t.Fatalf("SetAgent: %v", err)
	}

	got := store.GetAgent("agent-1")
	if got == nil {
		t.Fatal("agent-1 not found")
	}

	if got.ID != agent.ID {
		t.Errorf("ID = %q, want %q", got.ID, agent.ID)
	}
	if got.Team != agent.Team {
		t.Errorf("Team = %q, want %q", got.Team, agent.Team)
	}
	if got.Status != agent.Status {
		t.Errorf("Status = %q, want %q", got.Status, agent.Status)
	}
	if got.VMPID != agent.VMPID {
		t.Errorf("VMPID = %d, want %d", got.VMPID, agent.VMPID)
	}
	if got.VMCID != agent.VMCID {
		t.Errorf("VMCID = %d, want %d", got.VMCID, agent.VMCID)
	}
	if got.VMSocketPath != agent.VMSocketPath {
		t.Errorf("VMSocketPath = %q, want %q", got.VMSocketPath, agent.VMSocketPath)
	}
	if got.RootfsCopyPath != agent.RootfsCopyPath {
		t.Errorf("RootfsCopyPath = %q, want %q", got.RootfsCopyPath, agent.RootfsCopyPath)
	}
	if got.RestartCount != agent.RestartCount {
		t.Errorf("RestartCount = %d, want %d", got.RestartCount, agent.RestartCount)
	}
}

// ---------------------------------------------------------------------------
// State persistence: write, create new store from same file, verify identical
// ---------------------------------------------------------------------------

func TestStatePersistence_NewStoreFromSameFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Create first store and write agents.
	store1, err := NewStore(path, logger)
	if err != nil {
		t.Fatalf("NewStore (1): %v", err)
	}

	agents := []*AgentState{
		{ID: "alpha", Team: "ops", Status: AgentStatusRunning, VMPID: 100, VMCID: 10, RestartCount: 1, LastTransition: time.Now().Truncate(time.Millisecond)},
		{ID: "beta", Team: "dev", Status: AgentStatusStopped, RestartCount: 0, LastTransition: time.Now().Truncate(time.Millisecond)},
	}
	for _, a := range agents {
		if err := store1.SetAgent(a); err != nil {
			t.Fatalf("SetAgent(%s): %v", a.ID, err)
		}
	}
	store1.Close()

	// Create a second store from the same file path.
	store2, err := NewStore(path, logger)
	if err != nil {
		t.Fatalf("NewStore (2): %v", err)
	}
	defer store2.Close()

	for _, want := range agents {
		got := store2.GetAgent(want.ID)
		if got == nil {
			t.Fatalf("agent %q not found in store2", want.ID)
		}
		if got.ID != want.ID {
			t.Errorf("agent %q: ID = %q, want %q", want.ID, got.ID, want.ID)
		}
		if got.Team != want.Team {
			t.Errorf("agent %q: Team = %q, want %q", want.ID, got.Team, want.Team)
		}
		if got.Status != want.Status {
			t.Errorf("agent %q: Status = %q, want %q", want.ID, got.Status, want.Status)
		}
		if got.VMPID != want.VMPID {
			t.Errorf("agent %q: VMPID = %d, want %d", want.ID, got.VMPID, want.VMPID)
		}
		if got.VMCID != want.VMCID {
			t.Errorf("agent %q: VMCID = %d, want %d", want.ID, got.VMCID, want.VMCID)
		}
		if got.RestartCount != want.RestartCount {
			t.Errorf("agent %q: RestartCount = %d, want %d", want.ID, got.RestartCount, want.RestartCount)
		}
	}
}

// ---------------------------------------------------------------------------
// State recovery: agent in RUNNING is loaded correctly
// ---------------------------------------------------------------------------

func TestStateRecovery_RunningAgentLoadsCorrectly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	store1, err := NewStore(path, logger)
	if err != nil {
		t.Fatalf("NewStore (1): %v", err)
	}

	now := time.Now().Truncate(time.Millisecond)
	agent := &AgentState{
		ID:             "running-agent",
		Team:           "platform",
		Status:         AgentStatusRunning,
		VMPID:          9999,
		VMCID:          50,
		VMSocketPath:   "/var/run/fc-running.sock",
		RootfsCopyPath: "/var/lib/rootfs-running.ext4",
		RestartCount:   2,
		LastTransition: now,
		StartedAt:      now.Add(-10 * time.Minute),
		Error:          "",
	}
	if err := store1.SetAgent(agent); err != nil {
		t.Fatalf("SetAgent: %v", err)
	}
	store1.Close()

	// Simulate daemon restart by creating a new store.
	store2, err := NewStore(path, logger)
	if err != nil {
		t.Fatalf("NewStore (2): %v", err)
	}
	defer store2.Close()

	got := store2.GetAgent("running-agent")
	if got == nil {
		t.Fatal("running-agent not found after recovery")
	}
	if got.Status != AgentStatusRunning {
		t.Errorf("Status = %q, want %q", got.Status, AgentStatusRunning)
	}
	if got.VMPID != 9999 {
		t.Errorf("VMPID = %d, want 9999", got.VMPID)
	}
	if got.VMCID != 50 {
		t.Errorf("VMCID = %d, want 50", got.VMCID)
	}
	if got.VMSocketPath != "/var/run/fc-running.sock" {
		t.Errorf("VMSocketPath = %q, want %q", got.VMSocketPath, "/var/run/fc-running.sock")
	}
	if got.RestartCount != 2 {
		t.Errorf("RestartCount = %d, want 2", got.RestartCount)
	}
}

// ---------------------------------------------------------------------------
// GetAgent returns nil for missing agent
// ---------------------------------------------------------------------------

func TestGetAgent_ReturnsNilForMissing(t *testing.T) {
	store, _ := testStore(t)

	got := store.GetAgent("nonexistent")
	if got != nil {
		t.Errorf("expected nil for nonexistent agent, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// GetAgent returns a copy (mutations don't affect store)
// ---------------------------------------------------------------------------

func TestGetAgent_ReturnsCopy(t *testing.T) {
	store, _ := testStore(t)

	agent := testAgentState("copy-test", "team", AgentStatusPending)
	if err := store.SetAgent(agent); err != nil {
		t.Fatalf("SetAgent: %v", err)
	}

	got := store.GetAgent("copy-test")
	got.Status = AgentStatusRunning
	got.Team = "mutated"

	// Re-read from store; should be unchanged.
	original := store.GetAgent("copy-test")
	if original.Status != AgentStatusPending {
		t.Errorf("Status was mutated through returned pointer: got %q, want %q", original.Status, AgentStatusPending)
	}
	if original.Team != "team" {
		t.Errorf("Team was mutated through returned pointer: got %q, want %q", original.Team, "team")
	}
}

// ---------------------------------------------------------------------------
// AllAgents returns sorted by ID
// ---------------------------------------------------------------------------

func TestAllAgents_SortedByID(t *testing.T) {
	store, _ := testStore(t)

	// Insert in non-alphabetical order.
	ids := []string{"charlie", "alpha", "bravo", "delta"}
	for _, id := range ids {
		if err := store.SetAgent(testAgentState(id, "", AgentStatusPending)); err != nil {
			t.Fatalf("SetAgent(%s): %v", id, err)
		}
	}

	agents := store.AllAgents()
	if len(agents) != 4 {
		t.Fatalf("AllAgents returned %d agents, want 4", len(agents))
	}

	expected := []string{"alpha", "bravo", "charlie", "delta"}
	for i, want := range expected {
		if agents[i].ID != want {
			t.Errorf("AllAgents()[%d].ID = %q, want %q", i, agents[i].ID, want)
		}
	}
}

func TestAllAgents_EmptyStore(t *testing.T) {
	store, _ := testStore(t)

	agents := store.AllAgents()
	if len(agents) != 0 {
		t.Errorf("AllAgents on empty store returned %d, want 0", len(agents))
	}
}

func TestAllAgents_ReturnsCopies(t *testing.T) {
	store, _ := testStore(t)

	if err := store.SetAgent(testAgentState("agent-a", "team", AgentStatusRunning)); err != nil {
		t.Fatalf("SetAgent: %v", err)
	}

	agents := store.AllAgents()
	agents[0].Status = AgentStatusFailed

	// Re-read; should be unchanged.
	got := store.GetAgent("agent-a")
	if got.Status != AgentStatusRunning {
		t.Errorf("AllAgents returned mutable reference: Status = %q, want %q", got.Status, AgentStatusRunning)
	}
}

// ---------------------------------------------------------------------------
// RemoveAgent removes and persists
// ---------------------------------------------------------------------------

func TestRemoveAgent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	store, err := NewStore(path, logger)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	agent := testAgentState("removable", "team", AgentStatusStopped)
	if err := store.SetAgent(agent); err != nil {
		t.Fatalf("SetAgent: %v", err)
	}

	// Verify it exists.
	if got := store.GetAgent("removable"); got == nil {
		t.Fatal("agent not found before removal")
	}

	// Remove it.
	if err := store.RemoveAgent("removable"); err != nil {
		t.Fatalf("RemoveAgent: %v", err)
	}

	// Should be gone from memory.
	if got := store.GetAgent("removable"); got != nil {
		t.Error("agent still in memory after removal")
	}
	store.Close()

	// Should be gone from disk.
	store2, err := NewStore(path, logger)
	if err != nil {
		t.Fatalf("NewStore after removal: %v", err)
	}
	defer store2.Close()
	if got := store2.GetAgent("removable"); got != nil {
		t.Error("agent still on disk after removal")
	}
}

func TestRemoveAgent_NotFound(t *testing.T) {
	store, _ := testStore(t)

	err := store.RemoveAgent("ghost")
	if err == nil {
		t.Fatal("expected error removing nonexistent agent, got nil")
	}
}

// ---------------------------------------------------------------------------
// Database file exists after write
// ---------------------------------------------------------------------------

func TestDatabaseFile_Exists(t *testing.T) {
	store, path := testStore(t)

	agent := testAgentState("db-test", "infra", AgentStatusCreating)
	agent.VMPID = 555
	agent.VMCID = 88
	if err := store.SetAgent(agent); err != nil {
		t.Fatalf("SetAgent: %v", err)
	}

	// Verify the file exists.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("database file does not exist after write: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("database file is empty after write")
	}
}

// ---------------------------------------------------------------------------
// Multiple agents can coexist
// ---------------------------------------------------------------------------

func TestMultipleAgents(t *testing.T) {
	store, _ := testStore(t)

	for i, id := range []string{"a1", "a2", "a3"} {
		agent := testAgentState(id, "team", AgentStatusPending)
		agent.RestartCount = i
		if err := store.SetAgent(agent); err != nil {
			t.Fatalf("SetAgent(%s): %v", id, err)
		}
	}

	all := store.AllAgents()
	if len(all) != 3 {
		t.Fatalf("AllAgents returned %d, want 3", len(all))
	}

	for i, id := range []string{"a1", "a2", "a3"} {
		got := store.GetAgent(id)
		if got == nil {
			t.Fatalf("GetAgent(%s) = nil", id)
		}
		if got.RestartCount != i {
			t.Errorf("agent %s RestartCount = %d, want %d", id, got.RestartCount, i)
		}
	}
}

// ---------------------------------------------------------------------------
// SetAgent overwrites existing agent
// ---------------------------------------------------------------------------

func TestSetAgent_OverwritesExisting(t *testing.T) {
	store, _ := testStore(t)

	agent := testAgentState("overwrite", "team-a", AgentStatusPending)
	if err := store.SetAgent(agent); err != nil {
		t.Fatalf("SetAgent (1): %v", err)
	}

	// Overwrite with updated fields using a valid transition (PENDING -> CREATING).
	updated := testAgentState("overwrite", "team-b", AgentStatusCreating)
	updated.VMPID = 777
	if err := store.SetAgent(updated); err != nil {
		t.Fatalf("SetAgent (2): %v", err)
	}

	got := store.GetAgent("overwrite")
	if got == nil {
		t.Fatal("agent not found after overwrite")
	}
	if got.Team != "team-b" {
		t.Errorf("Team = %q, want %q", got.Team, "team-b")
	}
	if got.Status != AgentStatusCreating {
		t.Errorf("Status = %q, want %q", got.Status, AgentStatusCreating)
	}
	if got.VMPID != 777 {
		t.Errorf("VMPID = %d, want 777", got.VMPID)
	}

	// Still only one agent.
	all := store.AllAgents()
	if len(all) != 1 {
		t.Errorf("AllAgents returned %d, want 1", len(all))
	}
}

func TestSetAgent_RejectsInvalidTransition(t *testing.T) {
	store, _ := testStore(t)

	// Insert agent in PENDING status.
	agent := testAgentState("bad-transition", "team-a", AgentStatusPending)
	if err := store.SetAgent(agent); err != nil {
		t.Fatalf("SetAgent (initial): %v", err)
	}

	// Attempt invalid transition: PENDING -> RUNNING (skips CREATING, STARTING).
	updated := testAgentState("bad-transition", "team-a", AgentStatusRunning)
	if err := store.SetAgent(updated); err == nil {
		t.Fatal("expected error for invalid transition PENDING -> RUNNING, got nil")
	}

	// Verify the original status is unchanged.
	got := store.GetAgent("bad-transition")
	if got.Status != AgentStatusPending {
		t.Errorf("Status = %q after rejected transition, want %q", got.Status, AgentStatusPending)
	}
}

func TestSetAgent_AllowsSameStatus(t *testing.T) {
	store, _ := testStore(t)

	// Insert agent in RUNNING status.
	agent := testAgentState("same-status", "team-a", AgentStatusRunning)
	agent.RestartCount = 0
	if err := store.SetAgent(agent); err != nil {
		t.Fatalf("SetAgent (initial): %v", err)
	}

	// Update fields without changing status - should succeed.
	updated := testAgentState("same-status", "team-a", AgentStatusRunning)
	updated.RestartCount = 5
	if err := store.SetAgent(updated); err != nil {
		t.Fatalf("SetAgent (same status): %v", err)
	}

	got := store.GetAgent("same-status")
	if got.RestartCount != 5 {
		t.Errorf("RestartCount = %d, want 5", got.RestartCount)
	}
}

func TestSetAgent_AllowsAnyInitialStatus(t *testing.T) {
	store, _ := testStore(t)

	// New agents should be accepted with any initial status.
	statuses := []AgentStatus{
		AgentStatusPending,
		AgentStatusRunning,
		AgentStatusStopped,
		AgentStatusFailed,
		AgentStatusCreating,
	}
	for i, status := range statuses {
		id := fmt.Sprintf("new-agent-%d", i)
		agent := testAgentState(id, "team-a", status)
		if err := store.SetAgent(agent); err != nil {
			t.Errorf("SetAgent(%s, %s): unexpected error: %v", id, status, err)
		}
	}
}

// ---------------------------------------------------------------------------
// NewStore with nonexistent directory creates it
// ---------------------------------------------------------------------------

func TestNewStore_CreatesDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "nested", "state.db")
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	store, err := NewStore(path, logger)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	if err := store.SetAgent(testAgentState("dir-test", "", AgentStatusPending)); err != nil {
		t.Fatalf("SetAgent should work after directory creation: %v", err)
	}

	// Verify file exists.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database file not created: %v", err)
	}
}

// ---------------------------------------------------------------------------
// AgentState with Error field persists correctly
// ---------------------------------------------------------------------------

func TestAgentState_ErrorFieldPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	store, err := NewStore(path, logger)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	agent := &AgentState{
		ID:             "failed-agent",
		Status:         AgentStatusFailed,
		Error:          "VM process exited unexpectedly with code 137",
		LastTransition: time.Now().Truncate(time.Millisecond),
	}
	if err := store.SetAgent(agent); err != nil {
		t.Fatalf("SetAgent: %v", err)
	}
	store.Close()

	// Reload from disk.
	store2, err := NewStore(path, logger)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store2.Close()

	got := store2.GetAgent("failed-agent")
	if got == nil {
		t.Fatal("failed-agent not found after reload")
	}
	if got.Error != "VM process exited unexpectedly with code 137" {
		t.Errorf("Error = %q, want %q", got.Error, "VM process exited unexpectedly with code 137")
	}
}

// ===========================================================================
// Node management tests
// ===========================================================================

func testNodeState(id string, tier types.NodeTier, status types.NodeStatus) *types.NodeState {
	return &types.NodeState{
		ID:       id,
		Tier:     tier,
		Arch:     "amd64",
		Hostname: id + "-host",
		Status:   status,
		Resources: types.NodeResources{
			MemoryTotal: 8 * 1024 * 1024 * 1024,
			CPUCount:    4,
			KVMAvail:    tier == types.NodeTier1,
		},
		JoinedAt:      time.Now().Truncate(time.Millisecond),
		LastHeartbeat: time.Now().Truncate(time.Millisecond),
	}
}

// ---------------------------------------------------------------------------
// SetNode / GetNode round trip
// ---------------------------------------------------------------------------

func TestSetNode_GetNode(t *testing.T) {
	store, _ := testStore(t)

	node := testNodeState("node-1", types.NodeTier1, types.NodeStatusOnline)
	node.Labels = map[string]string{"hive.io/arch": "amd64"}

	if err := store.SetNode(node); err != nil {
		t.Fatalf("SetNode: %v", err)
	}

	got := store.GetNode("node-1")
	if got == nil {
		t.Fatal("GetNode returned nil")
	}
	if got.ID != "node-1" {
		t.Errorf("ID = %q, want %q", got.ID, "node-1")
	}
	if got.Tier != types.NodeTier1 {
		t.Errorf("Tier = %d, want %d", got.Tier, types.NodeTier1)
	}
	if got.Status != types.NodeStatusOnline {
		t.Errorf("Status = %q, want %q", got.Status, types.NodeStatusOnline)
	}
	if got.Arch != "amd64" {
		t.Errorf("Arch = %q, want %q", got.Arch, "amd64")
	}
	if got.Labels["hive.io/arch"] != "amd64" {
		t.Errorf("Labels[hive.io/arch] = %q, want %q", got.Labels["hive.io/arch"], "amd64")
	}
}

// ---------------------------------------------------------------------------
// GetNode returns nil for missing node
// ---------------------------------------------------------------------------

func TestGetNode_ReturnsNilForMissing(t *testing.T) {
	store, _ := testStore(t)

	got := store.GetNode("nonexistent")
	if got != nil {
		t.Errorf("expected nil for nonexistent node, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// GetNode returns a copy
// ---------------------------------------------------------------------------

func TestGetNode_ReturnsCopy(t *testing.T) {
	store, _ := testStore(t)

	node := testNodeState("copy-node", types.NodeTier1, types.NodeStatusOnline)
	if err := store.SetNode(node); err != nil {
		t.Fatalf("SetNode: %v", err)
	}

	got := store.GetNode("copy-node")
	got.Status = types.NodeStatusOffline
	got.Hostname = "mutated"

	original := store.GetNode("copy-node")
	if original.Status != types.NodeStatusOnline {
		t.Errorf("Status was mutated through returned pointer: got %q, want %q", original.Status, types.NodeStatusOnline)
	}
	if original.Hostname != "copy-node-host" {
		t.Errorf("Hostname was mutated through returned pointer: got %q, want %q", original.Hostname, "copy-node-host")
	}
}

// ---------------------------------------------------------------------------
// RemoveNode
// ---------------------------------------------------------------------------

func TestRemoveNode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	store, err := NewStore(path, logger)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	node := testNodeState("removable-node", types.NodeTier2, types.NodeStatusOffline)
	if err := store.SetNode(node); err != nil {
		t.Fatalf("SetNode: %v", err)
	}

	if got := store.GetNode("removable-node"); got == nil {
		t.Fatal("node not found before removal")
	}

	if err := store.RemoveNode("removable-node"); err != nil {
		t.Fatalf("RemoveNode: %v", err)
	}

	if got := store.GetNode("removable-node"); got != nil {
		t.Error("node still in memory after removal")
	}
	store.Close()

	// Verify persistence: reload from disk.
	store2, err := NewStore(path, logger)
	if err != nil {
		t.Fatalf("NewStore after removal: %v", err)
	}
	defer store2.Close()
	if got := store2.GetNode("removable-node"); got != nil {
		t.Error("node still on disk after removal")
	}
}

func TestRemoveNode_NotFound(t *testing.T) {
	store, _ := testStore(t)

	err := store.RemoveNode("ghost")
	if err == nil {
		t.Fatal("expected error removing nonexistent node, got nil")
	}
}

// ---------------------------------------------------------------------------
// AllNodes returns sorted by ID
// ---------------------------------------------------------------------------

func TestAllNodes_SortedByID(t *testing.T) {
	store, _ := testStore(t)

	ids := []string{"zeta-node", "alpha-node", "mu-node"}
	for _, id := range ids {
		if err := store.SetNode(testNodeState(id, types.NodeTier1, types.NodeStatusOnline)); err != nil {
			t.Fatalf("SetNode(%s): %v", id, err)
		}
	}

	nodes := store.AllNodes()
	if len(nodes) != 3 {
		t.Fatalf("AllNodes returned %d, want 3", len(nodes))
	}

	expected := []string{"alpha-node", "mu-node", "zeta-node"}
	for i, want := range expected {
		if nodes[i].ID != want {
			t.Errorf("AllNodes()[%d].ID = %q, want %q", i, nodes[i].ID, want)
		}
	}
}

func TestAllNodes_EmptyStore(t *testing.T) {
	store, _ := testStore(t)

	nodes := store.AllNodes()
	if len(nodes) != 0 {
		t.Errorf("AllNodes on empty store returned %d, want 0", len(nodes))
	}
}

// ---------------------------------------------------------------------------
// Node persistence across store reloads
// ---------------------------------------------------------------------------

func TestNodePersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	store1, err := NewStore(path, logger)
	if err != nil {
		t.Fatalf("NewStore (1): %v", err)
	}

	node := testNodeState("persist-node", types.NodeTier1, types.NodeStatusOnline)
	node.Agents = []string{"agent-a", "agent-b"}
	if err := store1.SetNode(node); err != nil {
		t.Fatalf("SetNode: %v", err)
	}
	store1.Close()

	store2, err := NewStore(path, logger)
	if err != nil {
		t.Fatalf("NewStore (2): %v", err)
	}
	defer store2.Close()

	got := store2.GetNode("persist-node")
	if got == nil {
		t.Fatal("node not found after reload")
	}
	if got.Tier != types.NodeTier1 {
		t.Errorf("Tier = %d, want %d", got.Tier, types.NodeTier1)
	}
	if len(got.Agents) != 2 {
		t.Errorf("Agents count = %d, want 2", len(got.Agents))
	}
}

// ===========================================================================
// Token management tests
// ===========================================================================

func testToken(prefix, rawToken string, ttl time.Duration) *types.Token {
	tok := &types.Token{
		Prefix:    prefix,
		Hash:      HashToken(rawToken),
		CreatedAt: time.Now().UTC(),
	}
	if ttl > 0 {
		tok.ExpiresAt = tok.CreatedAt.Add(ttl)
	}
	return tok
}

// ---------------------------------------------------------------------------
// AddToken / ValidateToken
// ---------------------------------------------------------------------------

func TestAddToken_ValidateToken(t *testing.T) {
	store, _ := testStore(t)

	raw := "test-token-value-0123456789abcdef"
	tok := testToken("test-tok", raw, 24*time.Hour)

	if err := store.AddToken(tok); err != nil {
		t.Fatalf("AddToken: %v", err)
	}

	validated := store.ValidateToken(raw)
	if validated == nil {
		t.Fatal("ValidateToken returned nil for valid token")
	}
	if validated.Prefix != "test-tok" {
		t.Errorf("Prefix = %q, want %q", validated.Prefix, "test-tok")
	}
}

func TestValidateToken_WrongToken(t *testing.T) {
	store, _ := testStore(t)

	raw := "correct-token-value"
	tok := testToken("correct", raw, 24*time.Hour)
	if err := store.AddToken(tok); err != nil {
		t.Fatalf("AddToken: %v", err)
	}

	validated := store.ValidateToken("wrong-token-value")
	if validated != nil {
		t.Errorf("expected nil for wrong token, got %+v", validated)
	}
}

func TestValidateToken_ExpiredToken(t *testing.T) {
	store, _ := testStore(t)

	raw := "expiring-token"
	tok := testToken("expire", raw, 1*time.Millisecond)
	if err := store.AddToken(tok); err != nil {
		t.Fatalf("AddToken: %v", err)
	}

	time.Sleep(5 * time.Millisecond)

	validated := store.ValidateToken(raw)
	if validated != nil {
		t.Errorf("expected nil for expired token, got %+v", validated)
	}
}

func TestValidateToken_RevokedToken(t *testing.T) {
	store, _ := testStore(t)

	raw := "revokable-token"
	tok := testToken("revoke", raw, 24*time.Hour)
	if err := store.AddToken(tok); err != nil {
		t.Fatalf("AddToken: %v", err)
	}

	// Revoke it.
	if err := store.RevokeToken("revoke"); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}

	validated := store.ValidateToken(raw)
	if validated != nil {
		t.Errorf("expected nil for revoked token, got %+v", validated)
	}
}

func TestValidateToken_NoExpiry(t *testing.T) {
	store, _ := testStore(t)

	raw := "no-expiry-token"
	tok := testToken("noexp", raw, 0) // 0 = no TTL
	if err := store.AddToken(tok); err != nil {
		t.Fatalf("AddToken: %v", err)
	}

	validated := store.ValidateToken(raw)
	if validated == nil {
		t.Fatal("expected non-nil for token with no expiry")
	}
}

// ---------------------------------------------------------------------------
// RevokeToken
// ---------------------------------------------------------------------------

func TestRevokeToken_NotFound(t *testing.T) {
	store, _ := testStore(t)

	err := store.RevokeToken("nonexistent")
	if err == nil {
		t.Fatal("expected error revoking nonexistent token, got nil")
	}
}

// ---------------------------------------------------------------------------
// AllTokens
// ---------------------------------------------------------------------------

func TestAllTokens(t *testing.T) {
	store, _ := testStore(t)

	for i := 0; i < 3; i++ {
		raw := "token-" + string(rune('a'+i))
		tok := testToken("tok-"+string(rune('a'+i)), raw, 24*time.Hour)
		if err := store.AddToken(tok); err != nil {
			t.Fatalf("AddToken: %v", err)
		}
	}

	tokens := store.AllTokens()
	if len(tokens) != 3 {
		t.Fatalf("AllTokens returned %d, want 3", len(tokens))
	}
}

func TestAllTokens_ReturnsCopies(t *testing.T) {
	store, _ := testStore(t)

	raw := "copy-test-token"
	tok := testToken("copytest", raw, 24*time.Hour)
	if err := store.AddToken(tok); err != nil {
		t.Fatalf("AddToken: %v", err)
	}

	tokens := store.AllTokens()
	tokens[0].Revoked = true

	// Re-read; should be unchanged.
	validated := store.ValidateToken(raw)
	if validated == nil {
		t.Fatal("token was mutated through AllTokens returned slice")
	}
}

// ---------------------------------------------------------------------------
// Token persistence across store reloads
// ---------------------------------------------------------------------------

func TestTokenPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	store1, err := NewStore(path, logger)
	if err != nil {
		t.Fatalf("NewStore (1): %v", err)
	}

	raw := "persist-token-value"
	tok := testToken("persist", raw, 24*time.Hour)
	if err := store1.AddToken(tok); err != nil {
		t.Fatalf("AddToken: %v", err)
	}
	store1.Close()

	store2, err := NewStore(path, logger)
	if err != nil {
		t.Fatalf("NewStore (2): %v", err)
	}
	defer store2.Close()

	validated := store2.ValidateToken(raw)
	if validated == nil {
		t.Fatal("token not found after store reload")
	}
	if validated.Prefix != "persist" {
		t.Errorf("Prefix = %q, want %q", validated.Prefix, "persist")
	}
}

// ===========================================================================
// Capability Registry tests
// ===========================================================================

// ---------------------------------------------------------------------------
// RegisterCapabilities / GetCapabilityRegistry
// ---------------------------------------------------------------------------

func TestRegisterCapabilities(t *testing.T) {
	store, _ := testStore(t)

	caps := []types.AgentCapability{
		{Name: "read-sensor", Description: "Reads sensor data"},
		{Name: "toggle-led", Description: "Toggles an LED"},
	}

	if err := store.RegisterCapabilities("agent-1", "sensors", "tier2", "node-1", caps); err != nil {
		t.Fatalf("RegisterCapabilities: %v", err)
	}

	reg := store.GetCapabilityRegistry()
	if reg == nil {
		t.Fatal("GetCapabilityRegistry returned nil")
	}

	entry, ok := reg.Agents["agent-1"]
	if !ok {
		t.Fatal("agent-1 not found in capability registry")
	}
	if entry.TeamID != "sensors" {
		t.Errorf("TeamID = %q, want %q", entry.TeamID, "sensors")
	}
	if entry.Tier != "tier2" {
		t.Errorf("Tier = %q, want %q", entry.Tier, "tier2")
	}
	if entry.NodeID != "node-1" {
		t.Errorf("NodeID = %q, want %q", entry.NodeID, "node-1")
	}
	if len(entry.Capabilities) != 2 {
		t.Fatalf("Capabilities count = %d, want 2", len(entry.Capabilities))
	}
	if entry.Capabilities[0].Name != "read-sensor" {
		t.Errorf("Capabilities[0].Name = %q, want %q", entry.Capabilities[0].Name, "read-sensor")
	}
	if entry.Capabilities[1].Name != "toggle-led" {
		t.Errorf("Capabilities[1].Name = %q, want %q", entry.Capabilities[1].Name, "toggle-led")
	}
}

// ---------------------------------------------------------------------------
// DeregisterCapabilities
// ---------------------------------------------------------------------------

func TestDeregisterCapabilities(t *testing.T) {
	store, _ := testStore(t)

	caps := []types.AgentCapability{
		{Name: "some-cap", Description: "Test"},
	}
	if err := store.RegisterCapabilities("agent-x", "team-x", "tier1", "node-x", caps); err != nil {
		t.Fatalf("RegisterCapabilities: %v", err)
	}

	// Verify it exists.
	reg := store.GetCapabilityRegistry()
	if _, ok := reg.Agents["agent-x"]; !ok {
		t.Fatal("agent-x not found before deregister")
	}

	if err := store.DeregisterCapabilities("agent-x"); err != nil {
		t.Fatalf("DeregisterCapabilities: %v", err)
	}

	reg = store.GetCapabilityRegistry()
	if _, ok := reg.Agents["agent-x"]; ok {
		t.Error("agent-x still in registry after deregister")
	}
}

// ---------------------------------------------------------------------------
// GetCapabilityRegistry returns a copy
// ---------------------------------------------------------------------------

func TestGetCapabilityRegistry_ReturnsCopy(t *testing.T) {
	store, _ := testStore(t)

	caps := []types.AgentCapability{
		{Name: "copy-cap", Description: "Test copy"},
	}
	if err := store.RegisterCapabilities("copy-agent", "team", "tier1", "node", caps); err != nil {
		t.Fatalf("RegisterCapabilities: %v", err)
	}

	reg := store.GetCapabilityRegistry()
	// Mutate the returned copy.
	delete(reg.Agents, "copy-agent")

	// Re-read; should be unchanged.
	reg2 := store.GetCapabilityRegistry()
	if _, ok := reg2.Agents["copy-agent"]; !ok {
		t.Error("capability registry was mutated through returned copy")
	}
}

// ---------------------------------------------------------------------------
// Multiple agents in capability registry
// ---------------------------------------------------------------------------

func TestCapabilityRegistry_MultipleAgents(t *testing.T) {
	store, _ := testStore(t)

	agents := []struct {
		id     string
		team   string
		tier   string
		nodeID string
		caps   []types.AgentCapability
	}{
		{"agent-a", "sensors", "tier2", "node-1", []types.AgentCapability{{Name: "read-temp", Description: "Temperature"}}},
		{"agent-b", "sensors", "tier2", "node-2", []types.AgentCapability{{Name: "read-humidity", Description: "Humidity"}}},
		{"agent-c", "actuators", "tier3", "node-3", []types.AgentCapability{{Name: "toggle-motor", Description: "Motor"}}},
	}

	for _, a := range agents {
		if err := store.RegisterCapabilities(a.id, a.team, a.tier, a.nodeID, a.caps); err != nil {
			t.Fatalf("RegisterCapabilities(%s): %v", a.id, err)
		}
	}

	reg := store.GetCapabilityRegistry()
	if len(reg.Agents) != 3 {
		t.Fatalf("registry has %d agents, want 3", len(reg.Agents))
	}

	// Verify FindByCapability.
	tempAgents := reg.FindByCapability("read-temp")
	if len(tempAgents) != 1 || tempAgents[0] != "agent-a" {
		t.Errorf("FindByCapability(read-temp) = %v, want [agent-a]", tempAgents)
	}

	// Verify AllCapabilities.
	allCaps := reg.AllCapabilities()
	if len(allCaps) != 3 {
		t.Errorf("AllCapabilities has %d capabilities, want 3", len(allCaps))
	}
}

// ---------------------------------------------------------------------------
// Capability registry persistence
// ---------------------------------------------------------------------------

func TestCapabilityRegistryPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	store1, err := NewStore(path, logger)
	if err != nil {
		t.Fatalf("NewStore (1): %v", err)
	}

	caps := []types.AgentCapability{
		{Name: "persist-cap", Description: "Persisted capability"},
	}
	if err := store1.RegisterCapabilities("persist-agent", "team", "tier1", "node", caps); err != nil {
		t.Fatalf("RegisterCapabilities: %v", err)
	}
	store1.Close()

	store2, err := NewStore(path, logger)
	if err != nil {
		t.Fatalf("NewStore (2): %v", err)
	}
	defer store2.Close()

	reg := store2.GetCapabilityRegistry()
	entry, ok := reg.Agents["persist-agent"]
	if !ok {
		t.Fatal("persist-agent not found in registry after reload")
	}
	if len(entry.Capabilities) != 1 {
		t.Fatalf("Capabilities count = %d, want 1", len(entry.Capabilities))
	}
	if entry.Capabilities[0].Name != "persist-cap" {
		t.Errorf("Capabilities[0].Name = %q, want %q", entry.Capabilities[0].Name, "persist-cap")
	}
}

// ---------------------------------------------------------------------------
// RegisterCapabilities overwrites existing
// ---------------------------------------------------------------------------

func TestRegisterCapabilities_OverwritesExisting(t *testing.T) {
	store, _ := testStore(t)

	caps1 := []types.AgentCapability{{Name: "old-cap", Description: "Old"}}
	if err := store.RegisterCapabilities("overwrite-agent", "team", "tier1", "node", caps1); err != nil {
		t.Fatalf("RegisterCapabilities (1): %v", err)
	}

	caps2 := []types.AgentCapability{
		{Name: "new-cap-1", Description: "New 1"},
		{Name: "new-cap-2", Description: "New 2"},
	}
	if err := store.RegisterCapabilities("overwrite-agent", "team", "tier1", "node", caps2); err != nil {
		t.Fatalf("RegisterCapabilities (2): %v", err)
	}

	reg := store.GetCapabilityRegistry()
	entry := reg.Agents["overwrite-agent"]
	if len(entry.Capabilities) != 2 {
		t.Fatalf("Capabilities count = %d, want 2", len(entry.Capabilities))
	}
	if entry.Capabilities[0].Name != "new-cap-1" {
		t.Errorf("Capabilities[0].Name = %q, want %q", entry.Capabilities[0].Name, "new-cap-1")
	}
}

// ---------------------------------------------------------------------------
// HashToken produces consistent hashes
// ---------------------------------------------------------------------------

func TestHashToken_Consistent(t *testing.T) {
	raw := "test-raw-token-value"
	h1 := HashToken(raw)
	h2 := HashToken(raw)

	if h1 != h2 {
		t.Errorf("HashToken produced different hashes for same input: %q vs %q", h1, h2)
	}

	// Different input should produce different hash.
	h3 := HashToken("different-token-value")
	if h1 == h3 {
		t.Error("HashToken produced same hash for different inputs")
	}
}

// ---------------------------------------------------------------------------
// Concurrent state store access
// ---------------------------------------------------------------------------

func TestConcurrentSetAgent_NoRace(t *testing.T) {
	store, _ := testStore(t)

	// Seed initial agents.
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("agent-%d", i)
		if err := store.SetAgent(testAgentState(id, "team", AgentStatusPending)); err != nil {
			t.Fatalf("seeding agent %s: %v", id, err)
		}
	}

	// Concurrently update agents from multiple goroutines.
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			id := fmt.Sprintf("agent-%d", idx)
			for j := 0; j < 50; j++ {
				a := store.GetAgent(id)
				if a == nil {
					t.Errorf("GetAgent(%s) returned nil", id)
					return
				}
				a.RestartCount = j
				if err := store.SetAgent(a); err != nil {
					t.Errorf("SetAgent(%s): %v", id, err)
					return
				}
			}
		}(i)
	}
	wg.Wait()

	// Verify all agents exist and have reasonable state.
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("agent-%d", i)
		a := store.GetAgent(id)
		if a == nil {
			t.Errorf("agent %s missing after concurrent updates", id)
		}
	}
}

func TestConcurrentModifyAgent_NoLostUpdates(t *testing.T) {
	store, _ := testStore(t)

	if err := store.SetAgent(testAgentState("counter", "team", AgentStatusRunning)); err != nil {
		t.Fatal(err)
	}

	// Use ModifyAgent to atomically increment RestartCount from 50 goroutines.
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := store.ModifyAgent("counter", func(a *AgentState) error {
				a.RestartCount++
				return nil
			})
			if err != nil {
				t.Errorf("ModifyAgent: %v", err)
			}
		}()
	}
	wg.Wait()

	a := store.GetAgent("counter")
	if a == nil {
		t.Fatal("agent not found")
	}
	if a.RestartCount != 50 {
		t.Errorf("RestartCount = %d, want 50 (lost updates detected)", a.RestartCount)
	}
}

// ---------------------------------------------------------------------------
// Empty store initialization
// ---------------------------------------------------------------------------

func TestNewStore_EmptyDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	store, err := NewStore(path, logger)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	// Should have started with empty state.
	if len(store.AllAgents()) != 0 {
		t.Fatalf("expected empty agents, got %d", len(store.AllAgents()))
	}

	nodes := store.AllNodes()
	if len(nodes) != 0 {
		t.Errorf("expected 0 nodes, got %d", len(nodes))
	}

	tokens := store.AllTokens()
	if tokens == nil {
		t.Error("expected non-nil tokens slice")
	}
}

// ---------------------------------------------------------------------------
// Mixed state: agents, nodes, tokens, and capabilities coexist
// ---------------------------------------------------------------------------

func TestMixedState_Coexistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	store, err := NewStore(path, logger)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	// Add agent.
	if err := store.SetAgent(testAgentState("agent-mixed", "team", AgentStatusRunning)); err != nil {
		t.Fatalf("SetAgent: %v", err)
	}

	// Add node.
	if err := store.SetNode(testNodeState("node-mixed", types.NodeTier1, types.NodeStatusOnline)); err != nil {
		t.Fatalf("SetNode: %v", err)
	}

	// Add token.
	raw := "mixed-state-token"
	tok := testToken("mixed", raw, 24*time.Hour)
	if err := store.AddToken(tok); err != nil {
		t.Fatalf("AddToken: %v", err)
	}

	// Add capabilities.
	caps := []types.AgentCapability{{Name: "mixed-cap", Description: "Mixed"}}
	if err := store.RegisterCapabilities("cap-agent", "team", "tier1", "node", caps); err != nil {
		t.Fatalf("RegisterCapabilities: %v", err)
	}
	store.Close()

	// Reload and verify all data survives.
	store2, err := NewStore(path, logger)
	if err != nil {
		t.Fatalf("NewStore (reload): %v", err)
	}
	defer store2.Close()

	if got := store2.GetAgent("agent-mixed"); got == nil {
		t.Error("agent not found after reload")
	}
	if got := store2.GetNode("node-mixed"); got == nil {
		t.Error("node not found after reload")
	}
	if got := store2.ValidateToken(raw); got == nil {
		t.Error("token not valid after reload")
	}
	reg := store2.GetCapabilityRegistry()
	if _, ok := reg.Agents["cap-agent"]; !ok {
		t.Error("capability not found after reload")
	}
}
