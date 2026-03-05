// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build unit

package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/brmurrell3/hive/internal/types"
)

// ---------------------------------------------------------------------------
// Mock backend implementation
// ---------------------------------------------------------------------------

// mockInstance is a minimal Backend Instance implementation.
type mockInstance struct {
	id          string
	agentID     string
	backendName string
}

func (i *mockInstance) ID() string      { return i.id }
func (i *mockInstance) AgentID() string { return i.agentID }
func (i *mockInstance) Backend() string { return i.backendName }

// mockBackend is a controllable Backend for testing.
type mockBackend struct {
	mu        sync.Mutex
	name      string
	caps      BackendCaps
	available Resources
	allocated Resources

	// Injected errors for each operation.
	CreateErr  error
	StartErr   error
	StopErr    error
	DestroyErr error
	StatusErr  error
	LogsErr    error

	// Recorded calls for assertion.
	created   []string // agentIDs passed to Create
	started   []string // IDs passed to Start
	stopped   []string // IDs passed to Stop
	destroyed []string // IDs passed to Destroy

	// Status returned by Status().
	statusState string

	// Logs content returned by Logs().
	logsContent string
}

func newMockBackend(name string) *mockBackend {
	return &mockBackend{
		name:        name,
		statusState: "running",
		logsContent: "log line 1\nlog line 2\n",
		available:   Resources{MemoryMB: 8192, VCPUs: 8, DiskMB: 102400},
	}
}

func (b *mockBackend) Name() string              { return b.name }
func (b *mockBackend) Capabilities() BackendCaps { return b.caps }
func (b *mockBackend) Available() Resources      { return b.available }
func (b *mockBackend) Allocated() Resources {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.allocated
}

func (b *mockBackend) Create(ctx context.Context, spec *types.AgentManifest) (Instance, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.CreateErr != nil {
		return nil, b.CreateErr
	}
	b.created = append(b.created, spec.Metadata.ID)
	return &mockInstance{
		id:          fmt.Sprintf("inst-%s", spec.Metadata.ID),
		agentID:     spec.Metadata.ID,
		backendName: b.name,
	}, nil
}

func (b *mockBackend) Start(ctx context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.StartErr != nil {
		return b.StartErr
	}
	b.started = append(b.started, id)
	return nil
}

func (b *mockBackend) Stop(ctx context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.StopErr != nil {
		return b.StopErr
	}
	b.stopped = append(b.stopped, id)
	return nil
}

func (b *mockBackend) Destroy(ctx context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.DestroyErr != nil {
		return b.DestroyErr
	}
	b.destroyed = append(b.destroyed, id)
	return nil
}

func (b *mockBackend) Status(ctx context.Context, id string) (InstanceStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.StatusErr != nil {
		return InstanceStatus{}, b.StatusErr
	}
	return InstanceStatus{State: b.statusState}, nil
}

func (b *mockBackend) Logs(ctx context.Context, id string, opts LogOpts) (io.ReadCloser, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.LogsErr != nil {
		return nil, b.LogsErr
	}
	return io.NopCloser(strings.NewReader(b.logsContent)), nil
}

// callCounts returns create/start/stop/destroy counts under the lock.
func (b *mockBackend) callCounts() (created, started, stopped, destroyed int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.created), len(b.started), len(b.stopped), len(b.destroyed)
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func testManifest(id, backendName string) *types.AgentManifest {
	return &types.AgentManifest{
		APIVersion: "hive/v1",
		Kind:       "Agent",
		Metadata:   types.AgentMetadata{ID: id},
		Spec: types.AgentSpec{
			Runtime: types.AgentRuntime{
				Type:    "test",
				Backend: backendName,
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Registry tests
// ---------------------------------------------------------------------------

func TestRegistry_Register(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	b := newMockBackend("firecracker")

	if err := r.Register(b); err != nil {
		t.Fatalf("Register: unexpected error: %v", err)
	}
}

func TestRegistry_Get_Found(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	b := newMockBackend("firecracker")
	if err := r.Register(b); err != nil {
		t.Fatalf("Register: %v", err)
	}

	got, err := r.Get("firecracker")
	if err != nil {
		t.Fatalf("Get: unexpected error: %v", err)
	}
	if got.Name() != "firecracker" {
		t.Errorf("Get returned backend with name %q, want %q", got.Name(), "firecracker")
	}
}

func TestRegistry_Get_NotFound(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	_, err := r.Get("nonexistent")
	if err == nil {
		t.Fatal("Get: expected error for unknown backend, got nil")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("Get error = %q, expected it to mention the backend name", err.Error())
	}
}

func TestRegistry_DuplicateRegistration(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	b := newMockBackend("firecracker")

	if err := r.Register(b); err != nil {
		t.Fatalf("first Register: %v", err)
	}

	// Registering a second backend with the same name must fail.
	b2 := newMockBackend("firecracker")
	err := r.Register(b2)
	if err == nil {
		t.Fatal("second Register: expected error for duplicate name, got nil")
	}
	if !strings.Contains(err.Error(), "already registered") {
		t.Errorf("duplicate error = %q, expected 'already registered'", err.Error())
	}
}

func TestRegistry_List(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	// Empty registry.
	if names := r.List(); len(names) != 0 {
		t.Errorf("List on empty registry = %v, want []", names)
	}

	// Register two backends.
	for _, name := range []string{"process", "firecracker"} {
		if err := r.Register(newMockBackend(name)); err != nil {
			t.Fatalf("Register(%s): %v", name, err)
		}
	}

	names := r.List()
	if len(names) != 2 {
		t.Fatalf("List returned %d names, want 2: %v", len(names), names)
	}
	// List must be sorted.
	if names[0] != "firecracker" || names[1] != "process" {
		t.Errorf("List = %v, want [firecracker process]", names)
	}
}

func TestRegistry_MultipleBackends(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	names := []string{"firecracker", "process", "nspawn"}

	for _, name := range names {
		if err := r.Register(newMockBackend(name)); err != nil {
			t.Fatalf("Register(%s): %v", name, err)
		}
	}

	for _, name := range names {
		got, err := r.Get(name)
		if err != nil {
			t.Errorf("Get(%s): unexpected error: %v", name, err)
			continue
		}
		if got.Name() != name {
			t.Errorf("Get(%s).Name() = %q, want %q", name, got.Name(), name)
		}
	}
}

// ---------------------------------------------------------------------------
// AgentManager: StartAgent
// ---------------------------------------------------------------------------

func TestAgentManager_StartAgent_Success(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	b := newMockBackend("firecracker")
	if err := r.Register(b); err != nil {
		t.Fatalf("Register: %v", err)
	}

	mgr := NewAgentManager(r, "firecracker", testLogger())
	spec := testManifest("agent-1", "firecracker")

	if err := mgr.StartAgent(context.Background(), spec); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}

	created, started, _, _ := b.callCounts()
	if created != 1 {
		t.Errorf("Create called %d times, want 1", created)
	}
	if started != 1 {
		t.Errorf("Start called %d times, want 1", started)
	}
}

func TestAgentManager_StartAgent_UsesDefaultBackend(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	b := newMockBackend("firecracker")
	if err := r.Register(b); err != nil {
		t.Fatalf("Register: %v", err)
	}

	mgr := NewAgentManager(r, "firecracker", testLogger())

	// Manifest with no backend specified — should fall back to default.
	spec := testManifest("agent-default", "" /* empty backend */)

	if err := mgr.StartAgent(context.Background(), spec); err != nil {
		t.Fatalf("StartAgent with default backend: %v", err)
	}

	created, _, _, _ := b.callCounts()
	if created != 1 {
		t.Errorf("Create called %d times on default backend, want 1", created)
	}
}

func TestAgentManager_StartAgent_ExplicitBackendOverridesDefault(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	fc := newMockBackend("firecracker")
	proc := newMockBackend("process")
	if err := r.Register(fc); err != nil {
		t.Fatalf("Register(firecracker): %v", err)
	}
	if err := r.Register(proc); err != nil {
		t.Fatalf("Register(process): %v", err)
	}

	// Default is firecracker, but spec requests process.
	mgr := NewAgentManager(r, "firecracker", testLogger())
	spec := testManifest("agent-proc", "process")

	if err := mgr.StartAgent(context.Background(), spec); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}

	fcCreated, _, _, _ := fc.callCounts()
	procCreated, _, _, _ := proc.callCounts()

	if fcCreated != 0 {
		t.Errorf("firecracker Create called %d times, want 0", fcCreated)
	}
	if procCreated != 1 {
		t.Errorf("process Create called %d times, want 1", procCreated)
	}
}

func TestAgentManager_StartAgent_AlreadyManaged(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	b := newMockBackend("firecracker")
	if err := r.Register(b); err != nil {
		t.Fatalf("Register: %v", err)
	}

	mgr := NewAgentManager(r, "firecracker", testLogger())
	spec := testManifest("dup-agent", "firecracker")

	if err := mgr.StartAgent(context.Background(), spec); err != nil {
		t.Fatalf("StartAgent (first): %v", err)
	}

	// Second call with the same agent ID must fail.
	err := mgr.StartAgent(context.Background(), spec)
	if err == nil {
		t.Fatal("StartAgent (second): expected error for already-managed agent, got nil")
	}
}

func TestAgentManager_StartAgent_BackendNotFound(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	// No backends registered at all.
	mgr := NewAgentManager(r, "firecracker", testLogger())
	spec := testManifest("orphan-agent", "firecracker")

	err := mgr.StartAgent(context.Background(), spec)
	if err == nil {
		t.Fatal("StartAgent: expected error when backend is not registered, got nil")
	}
}

func TestAgentManager_StartAgent_CreateError(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	b := newMockBackend("firecracker")
	b.CreateErr = errors.New("out of disk space")
	if err := r.Register(b); err != nil {
		t.Fatalf("Register: %v", err)
	}

	mgr := NewAgentManager(r, "firecracker", testLogger())
	spec := testManifest("fail-create", "firecracker")

	err := mgr.StartAgent(context.Background(), spec)
	if err == nil {
		t.Fatal("StartAgent: expected error when Create fails, got nil")
	}
	if !strings.Contains(err.Error(), "out of disk space") {
		t.Errorf("error = %q, expected it to contain the original error message", err.Error())
	}

	// Agent must not be tracked after a create failure.
	if name := mgr.BackendForAgent("fail-create"); name != "" {
		t.Errorf("BackendForAgent = %q after failed create, want empty", name)
	}
}

func TestAgentManager_StartAgent_StartError_CleansUp(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	b := newMockBackend("firecracker")
	b.StartErr = errors.New("kernel panic")
	if err := r.Register(b); err != nil {
		t.Fatalf("Register: %v", err)
	}

	mgr := NewAgentManager(r, "firecracker", testLogger())
	spec := testManifest("fail-start", "firecracker")

	err := mgr.StartAgent(context.Background(), spec)
	if err == nil {
		t.Fatal("StartAgent: expected error when Start fails, got nil")
	}

	// Destroy must have been called to clean up after the failed start.
	_, _, _, destroyed := b.callCounts()
	if destroyed != 1 {
		t.Errorf("Destroy called %d times after Start failure, want 1", destroyed)
	}

	// Agent must not be tracked after cleanup.
	if name := mgr.BackendForAgent("fail-start"); name != "" {
		t.Errorf("BackendForAgent = %q after failed start, want empty", name)
	}
}

// ---------------------------------------------------------------------------
// AgentManager: StopAgent
// ---------------------------------------------------------------------------

func TestAgentManager_StopAgent_Success(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	b := newMockBackend("firecracker")
	if err := r.Register(b); err != nil {
		t.Fatalf("Register: %v", err)
	}

	mgr := NewAgentManager(r, "firecracker", testLogger())
	spec := testManifest("stop-me", "firecracker")

	if err := mgr.StartAgent(context.Background(), spec); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}

	if err := mgr.StopAgent(context.Background(), "stop-me"); err != nil {
		t.Fatalf("StopAgent: %v", err)
	}

	_, _, stopped, _ := b.callCounts()
	if stopped != 1 {
		t.Errorf("Stop called %d times, want 1", stopped)
	}
}

func TestAgentManager_StopAgent_UnknownAgent(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	mgr := NewAgentManager(r, "firecracker", testLogger())

	err := mgr.StopAgent(context.Background(), "ghost")
	if err == nil {
		t.Fatal("StopAgent: expected error for unknown agent, got nil")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error = %q, expected it to mention the agent ID", err.Error())
	}
}

func TestAgentManager_StopAgent_BackendError(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	b := newMockBackend("firecracker")
	if err := r.Register(b); err != nil {
		t.Fatalf("Register: %v", err)
	}

	mgr := NewAgentManager(r, "firecracker", testLogger())
	spec := testManifest("fail-stop", "firecracker")

	if err := mgr.StartAgent(context.Background(), spec); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}

	b.StopErr = errors.New("SIGKILL failed")
	err := mgr.StopAgent(context.Background(), "fail-stop")
	if err == nil {
		t.Fatal("StopAgent: expected error from backend, got nil")
	}
}

// ---------------------------------------------------------------------------
// AgentManager: DestroyAgent
// ---------------------------------------------------------------------------

func TestAgentManager_DestroyAgent_Success(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	b := newMockBackend("firecracker")
	if err := r.Register(b); err != nil {
		t.Fatalf("Register: %v", err)
	}

	mgr := NewAgentManager(r, "firecracker", testLogger())
	spec := testManifest("destroy-me", "firecracker")

	if err := mgr.StartAgent(context.Background(), spec); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}

	if err := mgr.DestroyAgent(context.Background(), "destroy-me"); err != nil {
		t.Fatalf("DestroyAgent: %v", err)
	}

	_, _, _, destroyed := b.callCounts()
	if destroyed != 1 {
		t.Errorf("Destroy called %d times, want 1", destroyed)
	}

	// Agent must be un-tracked after destroy.
	if name := mgr.BackendForAgent("destroy-me"); name != "" {
		t.Errorf("BackendForAgent = %q after destroy, want empty", name)
	}
}

func TestAgentManager_DestroyAgent_UnknownAgent(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	mgr := NewAgentManager(r, "firecracker", testLogger())

	err := mgr.DestroyAgent(context.Background(), "nobody")
	if err == nil {
		t.Fatal("DestroyAgent: expected error for unknown agent, got nil")
	}
}

func TestAgentManager_DestroyAgent_BackendError(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	b := newMockBackend("firecracker")
	if err := r.Register(b); err != nil {
		t.Fatalf("Register: %v", err)
	}

	mgr := NewAgentManager(r, "firecracker", testLogger())
	spec := testManifest("fail-destroy", "firecracker")

	if err := mgr.StartAgent(context.Background(), spec); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}

	b.DestroyErr = errors.New("cgroups stuck")
	err := mgr.DestroyAgent(context.Background(), "fail-destroy")
	if err == nil {
		t.Fatal("DestroyAgent: expected error from backend, got nil")
	}

	// Agent must remain tracked when Destroy fails (no partial cleanup).
	if name := mgr.BackendForAgent("fail-destroy"); name == "" {
		t.Error("BackendForAgent is empty after failed Destroy, want agent to remain tracked")
	}
}

// ---------------------------------------------------------------------------
// AgentManager: Status
// ---------------------------------------------------------------------------

func TestAgentManager_Status_Running(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	b := newMockBackend("firecracker")
	b.statusState = "running"
	if err := r.Register(b); err != nil {
		t.Fatalf("Register: %v", err)
	}

	mgr := NewAgentManager(r, "firecracker", testLogger())
	spec := testManifest("status-agent", "firecracker")

	if err := mgr.StartAgent(context.Background(), spec); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}

	status, err := mgr.Status(context.Background(), "status-agent")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.State != "running" {
		t.Errorf("State = %q, want %q", status.State, "running")
	}
}

func TestAgentManager_Status_UnknownAgent(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	mgr := NewAgentManager(r, "firecracker", testLogger())

	status, err := mgr.Status(context.Background(), "ghost")
	if err == nil {
		t.Fatal("Status: expected error for unknown agent, got nil")
	}
	if status.State != "unknown" {
		t.Errorf("State = %q on error, want %q", status.State, "unknown")
	}
}

func TestAgentManager_Status_BackendError(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	b := newMockBackend("firecracker")
	if err := r.Register(b); err != nil {
		t.Fatalf("Register: %v", err)
	}

	mgr := NewAgentManager(r, "firecracker", testLogger())
	spec := testManifest("status-err", "firecracker")

	if err := mgr.StartAgent(context.Background(), spec); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}

	b.StatusErr = errors.New("cgroups unavailable")
	_, err := mgr.Status(context.Background(), "status-err")
	if err == nil {
		t.Fatal("Status: expected error from backend, got nil")
	}
}

// ---------------------------------------------------------------------------
// AgentManager: Logs
// ---------------------------------------------------------------------------

func TestAgentManager_Logs_Success(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	b := newMockBackend("firecracker")
	b.logsContent = "hello from agent\n"
	if err := r.Register(b); err != nil {
		t.Fatalf("Register: %v", err)
	}

	mgr := NewAgentManager(r, "firecracker", testLogger())
	spec := testManifest("log-agent", "firecracker")

	if err := mgr.StartAgent(context.Background(), spec); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}

	rc, err := mgr.Logs(context.Background(), "log-agent", LogOpts{Tail: 10})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(data) != "hello from agent\n" {
		t.Errorf("Logs content = %q, want %q", string(data), "hello from agent\n")
	}
}

func TestAgentManager_Logs_UnknownAgent(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	mgr := NewAgentManager(r, "firecracker", testLogger())

	_, err := mgr.Logs(context.Background(), "ghost", LogOpts{})
	if err == nil {
		t.Fatal("Logs: expected error for unknown agent, got nil")
	}
}

func TestAgentManager_Logs_BackendError(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	b := newMockBackend("firecracker")
	if err := r.Register(b); err != nil {
		t.Fatalf("Register: %v", err)
	}

	mgr := NewAgentManager(r, "firecracker", testLogger())
	spec := testManifest("log-err", "firecracker")

	if err := mgr.StartAgent(context.Background(), spec); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}

	b.LogsErr = errors.New("log file missing")
	_, err := mgr.Logs(context.Background(), "log-err", LogOpts{})
	if err == nil {
		t.Fatal("Logs: expected error from backend, got nil")
	}
}

// ---------------------------------------------------------------------------
// AgentManager: RestartAgent
// ---------------------------------------------------------------------------

func TestAgentManager_RestartAgent_TrackedAgent(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	b := newMockBackend("firecracker")
	if err := r.Register(b); err != nil {
		t.Fatalf("Register: %v", err)
	}

	mgr := NewAgentManager(r, "firecracker", testLogger())
	spec := testManifest("restart-me", "firecracker")

	if err := mgr.StartAgent(context.Background(), spec); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}

	if err := mgr.RestartAgent(context.Background(), spec); err != nil {
		t.Fatalf("RestartAgent: %v", err)
	}

	// Expect: 2x Create, 2x Start, 1x Stop + 1x Destroy (from the teardown before restart).
	created, started, stopped, destroyed := b.callCounts()
	if created != 2 {
		t.Errorf("Create called %d times, want 2", created)
	}
	if started != 2 {
		t.Errorf("Start called %d times, want 2", started)
	}
	if stopped != 1 {
		t.Errorf("Stop called %d times, want 1", stopped)
	}
	if destroyed != 1 {
		t.Errorf("Destroy called %d times, want 1", destroyed)
	}
}

func TestAgentManager_RestartAgent_UntrackedAgent(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	b := newMockBackend("firecracker")
	if err := r.Register(b); err != nil {
		t.Fatalf("Register: %v", err)
	}

	mgr := NewAgentManager(r, "firecracker", testLogger())
	spec := testManifest("fresh-restart", "firecracker")

	// RestartAgent on an agent that was never started should just start it.
	if err := mgr.RestartAgent(context.Background(), spec); err != nil {
		t.Fatalf("RestartAgent on untracked agent: %v", err)
	}

	created, started, _, destroyed := b.callCounts()
	if created != 1 {
		t.Errorf("Create called %d times, want 1", created)
	}
	if started != 1 {
		t.Errorf("Start called %d times, want 1", started)
	}
	if destroyed != 0 {
		t.Errorf("Destroy called %d times, want 0 (no prior instance)", destroyed)
	}
}

// ---------------------------------------------------------------------------
// AgentManager: BackendForAgent / RegisterAgent / RegisterAgentFromState
// ---------------------------------------------------------------------------

func TestAgentManager_BackendForAgent(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	b := newMockBackend("firecracker")
	if err := r.Register(b); err != nil {
		t.Fatalf("Register: %v", err)
	}

	mgr := NewAgentManager(r, "firecracker", testLogger())
	spec := testManifest("tracked", "firecracker")

	if err := mgr.StartAgent(context.Background(), spec); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}

	if name := mgr.BackendForAgent("tracked"); name != "firecracker" {
		t.Errorf("BackendForAgent = %q, want %q", name, "firecracker")
	}

	if name := mgr.BackendForAgent("unknown"); name != "" {
		t.Errorf("BackendForAgent for unknown = %q, want empty", name)
	}
}

func TestAgentManager_RegisterAgent(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	mgr := NewAgentManager(r, "firecracker", testLogger())

	// Simulate startup reconciliation: register an existing agent without starting.
	mgr.RegisterAgent("reconciled-agent", "firecracker")

	if name := mgr.BackendForAgent("reconciled-agent"); name != "firecracker" {
		t.Errorf("BackendForAgent = %q, want %q", name, "firecracker")
	}
}

func TestAgentManager_RegisterAgentFromState_WithName(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	mgr := NewAgentManager(r, "firecracker", testLogger())

	mgr.RegisterAgentFromState("from-state-agent", "process")

	if name := mgr.BackendForAgent("from-state-agent"); name != "process" {
		t.Errorf("BackendForAgent = %q, want %q", name, "process")
	}
}

func TestAgentManager_RegisterAgentFromState_EmptyFallsBackToDefault(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	mgr := NewAgentManager(r, "firecracker", testLogger())

	// Empty backend name must fall back to the default.
	mgr.RegisterAgentFromState("default-state-agent", "")

	if name := mgr.BackendForAgent("default-state-agent"); name != "firecracker" {
		t.Errorf("BackendForAgent = %q, want %q", name, "firecracker")
	}
}

// ---------------------------------------------------------------------------
// AgentManager: StopVM (VMAccess interface)
// ---------------------------------------------------------------------------

func TestAgentManager_StopVM_DelegatesToStopAgent(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	b := newMockBackend("firecracker")
	if err := r.Register(b); err != nil {
		t.Fatalf("Register: %v", err)
	}

	mgr := NewAgentManager(r, "firecracker", testLogger())
	spec := testManifest("vm-stop", "firecracker")

	if err := mgr.StartAgent(context.Background(), spec); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}

	if err := mgr.StopVM(context.Background(), "vm-stop"); err != nil {
		t.Fatalf("StopVM: %v", err)
	}

	_, _, stopped, _ := b.callCounts()
	if stopped != 1 {
		t.Errorf("Stop called %d times via StopVM, want 1", stopped)
	}
}

// ---------------------------------------------------------------------------
// AgentManager: multiple agents on multiple backends
// ---------------------------------------------------------------------------

func TestAgentManager_MultipleAgentsMultipleBackends(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	fc := newMockBackend("firecracker")
	proc := newMockBackend("process")
	if err := r.Register(fc); err != nil {
		t.Fatalf("Register(firecracker): %v", err)
	}
	if err := r.Register(proc); err != nil {
		t.Fatalf("Register(process): %v", err)
	}

	mgr := NewAgentManager(r, "firecracker", testLogger())

	fcAgents := []string{"fc-1", "fc-2"}
	procAgents := []string{"proc-1", "proc-2", "proc-3"}

	for _, id := range fcAgents {
		if err := mgr.StartAgent(context.Background(), testManifest(id, "firecracker")); err != nil {
			t.Fatalf("StartAgent(%s): %v", id, err)
		}
	}
	for _, id := range procAgents {
		if err := mgr.StartAgent(context.Background(), testManifest(id, "process")); err != nil {
			t.Fatalf("StartAgent(%s): %v", id, err)
		}
	}

	fcCreated, _, _, _ := fc.callCounts()
	procCreated, _, _, _ := proc.callCounts()

	if fcCreated != len(fcAgents) {
		t.Errorf("firecracker Create called %d times, want %d", fcCreated, len(fcAgents))
	}
	if procCreated != len(procAgents) {
		t.Errorf("process Create called %d times, want %d", procCreated, len(procAgents))
	}

	// Verify each agent is mapped to the correct backend.
	for _, id := range fcAgents {
		if name := mgr.BackendForAgent(id); name != "firecracker" {
			t.Errorf("BackendForAgent(%s) = %q, want firecracker", id, name)
		}
	}
	for _, id := range procAgents {
		if name := mgr.BackendForAgent(id); name != "process" {
			t.Errorf("BackendForAgent(%s) = %q, want process", id, name)
		}
	}
}

// ---------------------------------------------------------------------------
// BackendCaps and Resources helpers
// ---------------------------------------------------------------------------

func TestBackend_Capabilities(t *testing.T) {
	t.Parallel()
	b := newMockBackend("nspawn")
	b.caps = BackendCaps{
		Isolation:              "container",
		SupportsSnapshots:      false,
		SupportsNetworkPolicy:  true,
		SupportsMounts:         true,
		SupportsResourceLimits: true,
	}

	caps := b.Capabilities()
	if caps.Isolation != "container" {
		t.Errorf("Isolation = %q, want container", caps.Isolation)
	}
	if !caps.SupportsNetworkPolicy {
		t.Error("SupportsNetworkPolicy should be true")
	}
	if caps.SupportsSnapshots {
		t.Error("SupportsSnapshots should be false")
	}
}

func TestBackend_Resources(t *testing.T) {
	t.Parallel()
	b := newMockBackend("firecracker")
	b.available = Resources{MemoryMB: 16384, VCPUs: 16, DiskMB: 204800}

	avail := b.Available()
	if avail.MemoryMB != 16384 {
		t.Errorf("Available().MemoryMB = %d, want 16384", avail.MemoryMB)
	}
	if avail.VCPUs != 16 {
		t.Errorf("Available().VCPUs = %d, want 16", avail.VCPUs)
	}
	if avail.DiskMB != 204800 {
		t.Errorf("Available().DiskMB = %d, want 204800", avail.DiskMB)
	}
}

// ---------------------------------------------------------------------------
// Instance interface helpers
// ---------------------------------------------------------------------------

func TestInstance_Fields(t *testing.T) {
	t.Parallel()
	inst := &mockInstance{
		id:          "inst-abc",
		agentID:     "agent-abc",
		backendName: "firecracker",
	}

	if inst.ID() != "inst-abc" {
		t.Errorf("ID() = %q, want inst-abc", inst.ID())
	}
	if inst.AgentID() != "agent-abc" {
		t.Errorf("AgentID() = %q, want agent-abc", inst.AgentID())
	}
	if inst.Backend() != "firecracker" {
		t.Errorf("Backend() = %q, want firecracker", inst.Backend())
	}
}
