//go:build unit

package state

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func testStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store, err := NewStore(path, logger)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
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
		{"FAILED -> CREATING (manual restart)", AgentStatusFailed, AgentStatusCreating, false},

		// Invalid transitions.
		{"PENDING -> RUNNING (skip)", AgentStatusPending, AgentStatusRunning, true},
		{"PENDING -> STARTING (skip)", AgentStatusPending, AgentStatusStarting, true},
		{"PENDING -> STOPPED", AgentStatusPending, AgentStatusStopped, true},
		{"PENDING -> FAILED", AgentStatusPending, AgentStatusFailed, true},
		{"RUNNING -> CREATING (no direct restart)", AgentStatusRunning, AgentStatusCreating, true},
		{"RUNNING -> STOPPED (skip STOPPING)", AgentStatusRunning, AgentStatusStopped, true},
		{"RUNNING -> STARTING (backwards)", AgentStatusRunning, AgentStatusStarting, true},
		{"STOPPED -> RUNNING (skip)", AgentStatusStopped, AgentStatusRunning, true},
		{"STOPPED -> STARTING (skip)", AgentStatusStopped, AgentStatusStarting, true},
		{"FAILED -> RUNNING (skip)", AgentStatusFailed, AgentStatusRunning, true},
		{"FAILED -> STOPPED", AgentStatusFailed, AgentStatusStopped, true},
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
// state.json serialization / deserialization
// ---------------------------------------------------------------------------

func TestSetAgent_SerializationRoundTrip(t *testing.T) {
	store, path := testStore(t)

	now := time.Now().Truncate(time.Millisecond) // truncate for JSON precision
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

	// Read the file back and unmarshal manually.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading state file: %v", err)
	}

	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("unmarshalling state file: %v", err)
	}

	got, ok := st.Agents["agent-1"]
	if !ok {
		t.Fatal("agent-1 not found in deserialized state")
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
	if !got.LastTransition.Equal(agent.LastTransition) {
		t.Errorf("LastTransition = %v, want %v", got.LastTransition, agent.LastTransition)
	}
	if !got.StartedAt.Equal(agent.StartedAt) {
		t.Errorf("StartedAt = %v, want %v", got.StartedAt, agent.StartedAt)
	}
}

// ---------------------------------------------------------------------------
// State persistence: write, create new store from same file, verify identical
// ---------------------------------------------------------------------------

func TestStatePersistence_NewStoreFromSameFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
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

	// Create a second store from the same file path.
	store2, err := NewStore(path, logger)
	if err != nil {
		t.Fatalf("NewStore (2): %v", err)
	}

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
	path := filepath.Join(dir, "state.json")
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

	// Simulate daemon restart by creating a new store.
	store2, err := NewStore(path, logger)
	if err != nil {
		t.Fatalf("NewStore (2): %v", err)
	}

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
	if !got.StartedAt.Equal(agent.StartedAt) {
		t.Errorf("StartedAt = %v, want %v", got.StartedAt, agent.StartedAt)
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
	store, path := testStore(t)

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

	// Should be gone from disk.
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store2, err := NewStore(path, logger)
	if err != nil {
		t.Fatalf("NewStore after removal: %v", err)
	}
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
// Atomic write: file exists and is valid JSON after write
// ---------------------------------------------------------------------------

func TestAtomicWrite_ValidJSON(t *testing.T) {
	store, path := testStore(t)

	agent := testAgentState("atomic-test", "infra", AgentStatusCreating)
	agent.VMPID = 555
	agent.VMCID = 88
	if err := store.SetAgent(agent); err != nil {
		t.Fatalf("SetAgent: %v", err)
	}

	// Verify the file exists.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("state file does not exist after write: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("state file is empty after write")
	}

	// Verify it is valid JSON.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading state file: %v", err)
	}

	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("state file is not valid JSON: %v", err)
	}

	if len(st.Agents) != 1 {
		t.Errorf("expected 1 agent in state file, got %d", len(st.Agents))
	}
	if _, ok := st.Agents["atomic-test"]; !ok {
		t.Error("agent 'atomic-test' not found in state file")
	}
}

func TestAtomicWrite_NoTempFileLeftBehind(t *testing.T) {
	store, path := testStore(t)
	dir := filepath.Dir(path)

	if err := store.SetAgent(testAgentState("cleanup", "", AgentStatusPending)); err != nil {
		t.Fatalf("SetAgent: %v", err)
	}

	// Check that no .tmp files remain in the directory.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading dir: %v", err)
	}

	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("temp file left behind: %s", e.Name())
		}
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

	// Overwrite with updated fields.
	updated := testAgentState("overwrite", "team-b", AgentStatusRunning)
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
	if got.Status != AgentStatusRunning {
		t.Errorf("Status = %q, want %q", got.Status, AgentStatusRunning)
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

// ---------------------------------------------------------------------------
// NewStore with nonexistent directory creates it on first save
// ---------------------------------------------------------------------------

func TestNewStore_CreatesDirectoryOnSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "nested", "state.json")
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	store, err := NewStore(path, logger)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	// Directory does not exist yet (only created on first save).
	if err := store.SetAgent(testAgentState("dir-test", "", AgentStatusPending)); err != nil {
		t.Fatalf("SetAgent should create dirs: %v", err)
	}

	// Verify file exists.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state file not created: %v", err)
	}
}

// ---------------------------------------------------------------------------
// NewStore with corrupt file returns error
// ---------------------------------------------------------------------------

func TestNewStore_CorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	// Write garbage to the state file.
	if err := os.WriteFile(path, []byte("{corrupt"), 0644); err != nil {
		t.Fatalf("writing corrupt file: %v", err)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	_, err := NewStore(path, logger)
	if err == nil {
		t.Fatal("expected error loading corrupt state file, got nil")
	}
}

// ---------------------------------------------------------------------------
// AgentState with Error field persists correctly
// ---------------------------------------------------------------------------

func TestAgentState_ErrorFieldPersists(t *testing.T) {
	store, path := testStore(t)

	agent := &AgentState{
		ID:             "failed-agent",
		Status:         AgentStatusFailed,
		Error:          "VM process exited unexpectedly with code 137",
		LastTransition: time.Now().Truncate(time.Millisecond),
	}
	if err := store.SetAgent(agent); err != nil {
		t.Fatalf("SetAgent: %v", err)
	}

	// Reload from disk.
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store2, err := NewStore(path, logger)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	got := store2.GetAgent("failed-agent")
	if got == nil {
		t.Fatal("failed-agent not found after reload")
	}
	if got.Error != "VM process exited unexpectedly with code 137" {
		t.Errorf("Error = %q, want %q", got.Error, "VM process exited unexpectedly with code 137")
	}
}
