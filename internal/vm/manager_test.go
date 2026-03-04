//go:build unit

package vm

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivehq/hive/internal/state"
	"github.com/hivehq/hive/internal/types"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func testManager(t *testing.T) (*Manager, *MockHypervisor, *state.Store) {
	t.Helper()
	dir := t.TempDir()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store, err := state.NewStore(filepath.Join(dir, "state.json"), logger)
	if err != nil {
		t.Fatal(err)
	}
	mock := NewMockHypervisor()
	mgr := NewManager(dir, store, logger, mock)

	// Set up the directory structure and dummy files that StartAgent expects.
	// It needs rootfs/vmlinux and rootfs/rootfs.ext4 to exist for the copy.
	rootfsDir := filepath.Join(dir, "rootfs")
	if err := os.MkdirAll(rootfsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfsDir, "vmlinux"), []byte("fake-kernel"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfsDir, "rootfs.ext4"), []byte("fake-rootfs"), 0644); err != nil {
		t.Fatal(err)
	}

	return mgr, mock, store
}

func testAgent(id string) *types.AgentManifest {
	return &types.AgentManifest{
		APIVersion: "hive/v1",
		Kind:       "Agent",
		Metadata:   types.AgentMetadata{ID: id},
		Spec: types.AgentSpec{
			Runtime:   types.AgentRuntime{Type: "openclaw"},
			Resources: types.AgentResources{Memory: "512Mi", VCPUs: 2},
		},
	}
}

// ---------------------------------------------------------------------------
// Start agent: creates and starts VM, agent reaches RUNNING
// ---------------------------------------------------------------------------

func TestStartAgent_Success(t *testing.T) {
	mgr, mock, store := testManager(t)
	agent := testAgent("agent-1")

	if err := mgr.StartAgent(agent); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}

	// Agent should be RUNNING in state.
	agentState := store.GetAgent("agent-1")
	if agentState == nil {
		t.Fatal("agent-1 not found in state after start")
	}
	if agentState.Status != state.AgentStatusRunning {
		t.Errorf("Status = %q, want %q", agentState.Status, state.AgentStatusRunning)
	}
	if agentState.StartedAt.IsZero() {
		t.Error("StartedAt should not be zero")
	}
	if agentState.VMSocketPath == "" {
		t.Error("VMSocketPath should be set")
	}
	if agentState.RootfsCopyPath == "" {
		t.Error("RootfsCopyPath should be set")
	}
	if agentState.VMCID == 0 {
		t.Error("VMCID should be non-zero")
	}
	// T1-01: Verify VMPID is set after start.
	if agentState.VMPID == 0 {
		t.Error("VMPID should be non-zero after start")
	}

	// Mock hypervisor should show 1 running VM.
	if mock.RunningCount() != 1 {
		t.Errorf("mock RunningCount = %d, want 1", mock.RunningCount())
	}
}

func TestStartAgent_SetsTeamFromManifest(t *testing.T) {
	mgr, _, store := testManager(t)
	agent := testAgent("team-agent")
	agent.Metadata.Team = "platform"

	if err := mgr.StartAgent(agent); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}

	agentState := store.GetAgent("team-agent")
	if agentState == nil {
		t.Fatal("agent not found")
	}
	if agentState.Team != "platform" {
		t.Errorf("Team = %q, want %q", agentState.Team, "platform")
	}
}

// ---------------------------------------------------------------------------
// Starting an already RUNNING agent returns error
// ---------------------------------------------------------------------------

func TestStartAgent_AlreadyRunning(t *testing.T) {
	mgr, _, _ := testManager(t)
	agent := testAgent("dup-agent")

	if err := mgr.StartAgent(agent); err != nil {
		t.Fatalf("StartAgent (first): %v", err)
	}

	// Starting the same agent again should fail.
	err := mgr.StartAgent(agent)
	if err == nil {
		t.Fatal("expected error starting already-running agent, got nil")
	}
}

func TestStartAgent_AlreadyCreating(t *testing.T) {
	mgr, _, store := testManager(t)

	// Manually put an agent into CREATING state.
	if err := store.SetAgent(&state.AgentState{
		ID:     "creating-agent",
		Status: state.AgentStatusCreating,
	}); err != nil {
		t.Fatalf("SetAgent: %v", err)
	}

	err := mgr.StartAgent(testAgent("creating-agent"))
	if err == nil {
		t.Fatal("expected error starting agent in CREATING state, got nil")
	}
}

// ---------------------------------------------------------------------------
// Stop agent: transitions to STOPPED
// ---------------------------------------------------------------------------

func TestStopAgent_Success(t *testing.T) {
	mgr, mock, store := testManager(t)
	agent := testAgent("stop-me")

	if err := mgr.StartAgent(agent); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}

	if err := mgr.StopAgent("stop-me"); err != nil {
		t.Fatalf("StopAgent: %v", err)
	}

	agentState := store.GetAgent("stop-me")
	if agentState == nil {
		t.Fatal("agent not found after stop")
	}
	if agentState.Status != state.AgentStatusStopped {
		t.Errorf("Status = %q, want %q", agentState.Status, state.AgentStatusStopped)
	}
	if agentState.VMPID != 0 {
		t.Errorf("VMPID = %d, want 0 after stop", agentState.VMPID)
	}

	// Mock hypervisor should show 0 running VMs.
	if mock.RunningCount() != 0 {
		t.Errorf("mock RunningCount = %d, want 0", mock.RunningCount())
	}
}

// ---------------------------------------------------------------------------
// Stop a STOPPED agent returns error
// ---------------------------------------------------------------------------

func TestStopAgent_AlreadyStopped(t *testing.T) {
	mgr, _, store := testManager(t)

	// Put an agent into STOPPED state directly.
	if err := store.SetAgent(&state.AgentState{
		ID:     "stopped-agent",
		Status: state.AgentStatusStopped,
	}); err != nil {
		t.Fatalf("SetAgent: %v", err)
	}

	err := mgr.StopAgent("stopped-agent")
	if err == nil {
		t.Fatal("expected error stopping already-stopped agent, got nil")
	}
}

func TestStopAgent_NotFound(t *testing.T) {
	mgr, _, _ := testManager(t)

	err := mgr.StopAgent("ghost")
	if err == nil {
		t.Fatal("expected error stopping nonexistent agent, got nil")
	}
}

func TestStopAgent_PendingAgent(t *testing.T) {
	mgr, _, store := testManager(t)

	if err := store.SetAgent(&state.AgentState{
		ID:     "pending-agent",
		Status: state.AgentStatusPending,
	}); err != nil {
		t.Fatalf("SetAgent: %v", err)
	}

	err := mgr.StopAgent("pending-agent")
	if err == nil {
		t.Fatal("expected error stopping PENDING agent, got nil")
	}
}

// ---------------------------------------------------------------------------
// Destroy a RUNNING agent: stops then destroys
// ---------------------------------------------------------------------------

func TestDestroyAgent_RunningAgent(t *testing.T) {
	mgr, mock, store := testManager(t)
	agent := testAgent("destroy-me")

	if err := mgr.StartAgent(agent); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}

	if mock.RunningCount() != 1 {
		t.Fatalf("expected 1 running VM before destroy, got %d", mock.RunningCount())
	}

	if err := mgr.DestroyAgent("destroy-me"); err != nil {
		t.Fatalf("DestroyAgent: %v", err)
	}

	// Agent should be completely removed from state.
	agentState := store.GetAgent("destroy-me")
	if agentState != nil {
		t.Error("agent should be removed from state after destroy")
	}

	// Mock hypervisor should show 0 running VMs.
	if mock.RunningCount() != 0 {
		t.Errorf("mock RunningCount = %d, want 0", mock.RunningCount())
	}
}

// ---------------------------------------------------------------------------
// Destroy removes all artifacts and state
// ---------------------------------------------------------------------------

func TestDestroyAgent_RemovesArtifacts(t *testing.T) {
	mgr, _, store := testManager(t)
	agent := testAgent("artifact-agent")

	if err := mgr.StartAgent(agent); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}

	agentState := store.GetAgent("artifact-agent")
	if agentState == nil {
		t.Fatal("agent not found after start")
	}

	rootfsCopy := agentState.RootfsCopyPath
	socketPath := agentState.VMSocketPath

	// Verify rootfs copy exists (StartAgent copies it).
	if _, err := os.Stat(rootfsCopy); err != nil {
		t.Fatalf("rootfs copy should exist after start: %v", err)
	}

	if err := mgr.DestroyAgent("artifact-agent"); err != nil {
		t.Fatalf("DestroyAgent: %v", err)
	}

	// Agent should be gone from state.
	if got := store.GetAgent("artifact-agent"); got != nil {
		t.Error("agent should be removed from state after destroy")
	}

	// Rootfs copy should be cleaned up.
	if _, err := os.Stat(rootfsCopy); !os.IsNotExist(err) {
		t.Errorf("rootfs copy should be removed, stat returned: %v", err)
	}

	// Socket file should be cleaned up (mock doesn't create a real socket,
	// but destroy should not error on missing file).
	_ = socketPath // verified via mock; no real socket to check

	// State directory should be gone.
	stateDir := filepath.Dir(socketPath)
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Errorf("agent state directory should be removed, stat returned: %v", err)
	}
}

func TestDestroyAgent_StoppedAgent(t *testing.T) {
	mgr, _, store := testManager(t)
	agent := testAgent("stopped-destroy")

	// Start and stop, then destroy.
	if err := mgr.StartAgent(agent); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	if err := mgr.StopAgent("stopped-destroy"); err != nil {
		t.Fatalf("StopAgent: %v", err)
	}

	if err := mgr.DestroyAgent("stopped-destroy"); err != nil {
		t.Fatalf("DestroyAgent: %v", err)
	}

	if got := store.GetAgent("stopped-destroy"); got != nil {
		t.Error("agent should be removed after destroy")
	}
}

func TestDestroyAgent_NotFound(t *testing.T) {
	mgr, _, _ := testManager(t)

	err := mgr.DestroyAgent("ghost")
	if err == nil {
		t.Fatal("expected error destroying nonexistent agent, got nil")
	}
}

// ---------------------------------------------------------------------------
// Reconcile on startup: RUNNING agent with dead process -> FAILED
// ---------------------------------------------------------------------------

func TestReconcileOnStartup_RunningAgentDeadProcess(t *testing.T) {
	mgr, _, store := testManager(t)

	// Manually insert an agent that was RUNNING with a PID the mock
	// does not know about (simulating a crashed process).
	if err := store.SetAgent(&state.AgentState{
		ID:     "crashed-agent",
		Status: state.AgentStatusRunning,
		VMPID:  99999, // mock has no such PID
		VMCID:  10,
	}); err != nil {
		t.Fatalf("SetAgent: %v", err)
	}

	if err := mgr.ReconcileOnStartup(); err != nil {
		t.Fatalf("ReconcileOnStartup: %v", err)
	}

	agentState := store.GetAgent("crashed-agent")
	if agentState == nil {
		t.Fatal("agent not found after reconciliation")
	}
	if agentState.Status != state.AgentStatusFailed {
		t.Errorf("Status = %q, want %q", agentState.Status, state.AgentStatusFailed)
	}
	if agentState.Error == "" {
		t.Error("Error should be set after reconciliation marks agent FAILED")
	}
	if agentState.VMPID != 0 {
		t.Errorf("VMPID = %d, want 0 after reconciliation", agentState.VMPID)
	}
}

func TestReconcileOnStartup_RunningAgentNoPID(t *testing.T) {
	mgr, _, store := testManager(t)

	// Agent in RUNNING state but with no PID recorded.
	if err := store.SetAgent(&state.AgentState{
		ID:     "no-pid-agent",
		Status: state.AgentStatusRunning,
		VMPID:  0,
	}); err != nil {
		t.Fatalf("SetAgent: %v", err)
	}

	if err := mgr.ReconcileOnStartup(); err != nil {
		t.Fatalf("ReconcileOnStartup: %v", err)
	}

	agentState := store.GetAgent("no-pid-agent")
	if agentState == nil {
		t.Fatal("agent not found after reconciliation")
	}
	if agentState.Status != state.AgentStatusFailed {
		t.Errorf("Status = %q, want %q", agentState.Status, state.AgentStatusFailed)
	}
}

func TestReconcileOnStartup_CreatingAgent(t *testing.T) {
	mgr, _, store := testManager(t)

	// Agent was in CREATING when daemon crashed.
	if err := store.SetAgent(&state.AgentState{
		ID:     "creating-agent",
		Status: state.AgentStatusCreating,
	}); err != nil {
		t.Fatalf("SetAgent: %v", err)
	}

	if err := mgr.ReconcileOnStartup(); err != nil {
		t.Fatalf("ReconcileOnStartup: %v", err)
	}

	agentState := store.GetAgent("creating-agent")
	if agentState == nil {
		t.Fatal("agent not found after reconciliation")
	}
	if agentState.Status != state.AgentStatusFailed {
		t.Errorf("Status = %q, want %q", agentState.Status, state.AgentStatusFailed)
	}
}

func TestReconcileOnStartup_StoppedAndFailedUntouched(t *testing.T) {
	mgr, _, store := testManager(t)

	// Agents in terminal states should not be changed.
	for _, s := range []struct {
		id     string
		status state.AgentStatus
	}{
		{"stopped-agent", state.AgentStatusStopped},
		{"failed-agent", state.AgentStatusFailed},
		{"pending-agent", state.AgentStatusPending},
	} {
		if err := store.SetAgent(&state.AgentState{
			ID:     s.id,
			Status: s.status,
		}); err != nil {
			t.Fatalf("SetAgent(%s): %v", s.id, err)
		}
	}

	if err := mgr.ReconcileOnStartup(); err != nil {
		t.Fatalf("ReconcileOnStartup: %v", err)
	}

	// All should retain their original status.
	for _, s := range []struct {
		id     string
		status state.AgentStatus
	}{
		{"stopped-agent", state.AgentStatusStopped},
		{"failed-agent", state.AgentStatusFailed},
		{"pending-agent", state.AgentStatusPending},
	} {
		agentState := store.GetAgent(s.id)
		if agentState == nil {
			t.Fatalf("agent %s not found", s.id)
		}
		if agentState.Status != s.status {
			t.Errorf("agent %s: Status = %q, want %q", s.id, agentState.Status, s.status)
		}
	}
}

func TestReconcileOnStartup_StoppingAgentDeadProcess(t *testing.T) {
	mgr, _, store := testManager(t)

	// Agent was mid-STOPPING when daemon crashed.
	if err := store.SetAgent(&state.AgentState{
		ID:     "stopping-agent",
		Status: state.AgentStatusStopping,
		VMPID:  88888,
	}); err != nil {
		t.Fatalf("SetAgent: %v", err)
	}

	if err := mgr.ReconcileOnStartup(); err != nil {
		t.Fatalf("ReconcileOnStartup: %v", err)
	}

	agentState := store.GetAgent("stopping-agent")
	if agentState == nil {
		t.Fatal("agent not found after reconciliation")
	}
	if agentState.Status != state.AgentStatusFailed {
		t.Errorf("Status = %q, want %q", agentState.Status, state.AgentStatusFailed)
	}
}

// ---------------------------------------------------------------------------
// Restart agent
// ---------------------------------------------------------------------------

func TestRestartAgent_RunningAgent(t *testing.T) {
	mgr, mock, store := testManager(t)
	agent := testAgent("restart-me")

	if err := mgr.StartAgent(agent); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}

	if err := mgr.RestartAgent("restart-me", agent); err != nil {
		t.Fatalf("RestartAgent: %v", err)
	}

	agentState := store.GetAgent("restart-me")
	if agentState == nil {
		t.Fatal("agent not found after restart")
	}
	if agentState.Status != state.AgentStatusRunning {
		t.Errorf("Status = %q, want %q", agentState.Status, state.AgentStatusRunning)
	}
	if agentState.RestartCount != 0 {
		t.Errorf("RestartCount = %d, want 0 (reset on explicit restart)", agentState.RestartCount)
	}

	// Should still have exactly 1 running VM.
	if mock.RunningCount() != 1 {
		t.Errorf("mock RunningCount = %d, want 1", mock.RunningCount())
	}
}

func TestRestartAgent_FailedAgent(t *testing.T) {
	mgr, _, store := testManager(t)

	// Put an agent into FAILED state.
	if err := store.SetAgent(&state.AgentState{
		ID:           "failed-restart",
		Status:       state.AgentStatusFailed,
		Error:        "some error",
		RestartCount: 5,
	}); err != nil {
		t.Fatalf("SetAgent: %v", err)
	}

	agent := testAgent("failed-restart")
	if err := mgr.RestartAgent("failed-restart", agent); err != nil {
		t.Fatalf("RestartAgent: %v", err)
	}

	agentState := store.GetAgent("failed-restart")
	if agentState == nil {
		t.Fatal("agent not found after restart")
	}
	if agentState.Status != state.AgentStatusRunning {
		t.Errorf("Status = %q, want %q", agentState.Status, state.AgentStatusRunning)
	}
	if agentState.Error != "" {
		t.Errorf("Error = %q, want empty after restart", agentState.Error)
	}
	if agentState.RestartCount != 0 {
		t.Errorf("RestartCount = %d, want 0 after explicit restart", agentState.RestartCount)
	}
}

// ---------------------------------------------------------------------------
// Error injection: hypervisor CreateVM fails
// ---------------------------------------------------------------------------

func TestStartAgent_CreateVMError(t *testing.T) {
	mgr, mock, store := testManager(t)
	mock.CreateErr = errors.New("hypervisor create failed")

	err := mgr.StartAgent(testAgent("fail-create"))
	if err == nil {
		t.Fatal("expected error when CreateVM fails, got nil")
	}

	// Agent should be in FAILED state.
	agentState := store.GetAgent("fail-create")
	if agentState == nil {
		t.Fatal("agent not found after failed create")
	}
	if agentState.Status != state.AgentStatusFailed {
		t.Errorf("Status = %q, want %q", agentState.Status, state.AgentStatusFailed)
	}
	if agentState.Error == "" {
		t.Error("Error should be set after failed create")
	}
}

func TestStartAgent_StartVMError(t *testing.T) {
	mgr, mock, store := testManager(t)
	mock.StartErr = errors.New("hypervisor start failed")

	err := mgr.StartAgent(testAgent("fail-start"))
	if err == nil {
		t.Fatal("expected error when StartVM fails, got nil")
	}

	agentState := store.GetAgent("fail-start")
	if agentState == nil {
		t.Fatal("agent not found after failed start")
	}
	if agentState.Status != state.AgentStatusFailed {
		t.Errorf("Status = %q, want %q", agentState.Status, state.AgentStatusFailed)
	}
}

func TestStopAgent_StopVMError(t *testing.T) {
	mgr, mock, store := testManager(t)

	// Start successfully first.
	if err := mgr.StartAgent(testAgent("fail-stop")); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}

	// Inject stop error.
	mock.StopErr = errors.New("hypervisor stop failed")

	err := mgr.StopAgent("fail-stop")
	if err == nil {
		t.Fatal("expected error when StopVM fails, got nil")
	}

	// Agent should be FAILED (failAgent is called on stop error).
	agentState := store.GetAgent("fail-stop")
	if agentState == nil {
		t.Fatal("agent not found after failed stop")
	}
	if agentState.Status != state.AgentStatusFailed {
		t.Errorf("Status = %q, want %q", agentState.Status, state.AgentStatusFailed)
	}
}

// ---------------------------------------------------------------------------
// Resource parsing: valid memory string
// ---------------------------------------------------------------------------

func TestStartAgent_ValidMemoryParsing(t *testing.T) {
	tests := []struct {
		name   string
		memory string
	}{
		{"mebibytes", "512Mi"},
		{"gibibytes", "1Gi"},
		{"megabytes", "256MB"},
		{"plain bytes", "536870912"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr, _, store := testManager(t)
			agent := testAgent("mem-" + tt.name)
			agent.Spec.Resources.Memory = tt.memory

			if err := mgr.StartAgent(agent); err != nil {
				t.Fatalf("StartAgent with memory=%q: %v", tt.memory, err)
			}

			agentState := store.GetAgent("mem-" + tt.name)
			if agentState == nil {
				t.Fatal("agent not found")
			}
			if agentState.Status != state.AgentStatusRunning {
				t.Errorf("Status = %q, want %q", agentState.Status, state.AgentStatusRunning)
			}
		})
	}
}

func TestStartAgent_InvalidMemory(t *testing.T) {
	mgr, _, store := testManager(t)
	agent := testAgent("bad-mem")
	agent.Spec.Resources.Memory = "not-a-number"

	err := mgr.StartAgent(agent)
	if err == nil {
		t.Fatal("expected error for invalid memory string, got nil")
	}

	// Agent should be FAILED.
	agentState := store.GetAgent("bad-mem")
	if agentState == nil {
		t.Fatal("agent not found after failed start")
	}
	if agentState.Status != state.AgentStatusFailed {
		t.Errorf("Status = %q, want %q", agentState.Status, state.AgentStatusFailed)
	}
}

func TestStartAgent_DefaultResources(t *testing.T) {
	mgr, _, store := testManager(t)
	agent := testAgent("default-res")
	agent.Spec.Resources = types.AgentResources{} // no memory or vcpus set

	if err := mgr.StartAgent(agent); err != nil {
		t.Fatalf("StartAgent with empty resources: %v", err)
	}

	agentState := store.GetAgent("default-res")
	if agentState == nil {
		t.Fatal("agent not found")
	}
	if agentState.Status != state.AgentStatusRunning {
		t.Errorf("Status = %q, want %q", agentState.Status, state.AgentStatusRunning)
	}
}

// ---------------------------------------------------------------------------
// Multiple agents get unique CIDs
// ---------------------------------------------------------------------------

func TestStartAgent_UniqueCIDs(t *testing.T) {
	mgr, _, store := testManager(t)

	ids := []string{"cid-a", "cid-b", "cid-c"}
	for _, id := range ids {
		if err := mgr.StartAgent(testAgent(id)); err != nil {
			t.Fatalf("StartAgent(%s): %v", id, err)
		}
	}

	cids := make(map[uint32]string)
	for _, id := range ids {
		agentState := store.GetAgent(id)
		if agentState == nil {
			t.Fatalf("agent %s not found", id)
		}
		if prev, exists := cids[agentState.VMCID]; exists {
			t.Errorf("CID %d is shared between %s and %s", agentState.VMCID, prev, id)
		}
		cids[agentState.VMCID] = id
	}
}

// ---------------------------------------------------------------------------
// Multiple agents: running count tracks correctly
// ---------------------------------------------------------------------------

func TestMultipleAgents_RunningCount(t *testing.T) {
	mgr, mock, _ := testManager(t)

	for _, id := range []string{"m1", "m2", "m3"} {
		if err := mgr.StartAgent(testAgent(id)); err != nil {
			t.Fatalf("StartAgent(%s): %v", id, err)
		}
	}

	if mock.RunningCount() != 3 {
		t.Errorf("RunningCount = %d, want 3", mock.RunningCount())
	}

	if err := mgr.StopAgent("m2"); err != nil {
		t.Fatalf("StopAgent(m2): %v", err)
	}

	if mock.RunningCount() != 2 {
		t.Errorf("RunningCount = %d, want 2 after stopping one", mock.RunningCount())
	}
}
