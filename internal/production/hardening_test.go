//go:build unit

package production

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/hivehq/hive/internal/state"
	"github.com/hivehq/hive/internal/types"
)

// --- Mock implementations ---

// mockStore implements StoreAccess for testing.
type mockStore struct {
	mu     sync.Mutex
	agents map[string]*state.AgentState
	nodes  []*types.NodeState
}

func newMockStore() *mockStore {
	return &mockStore{
		agents: make(map[string]*state.AgentState),
	}
}

func (m *mockStore) AllAgents() []*state.AgentState {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := make([]*state.AgentState, 0, len(m.agents))
	for _, a := range m.agents {
		cp := *a
		result = append(result, &cp)
	}
	return result
}

func (m *mockStore) GetAgent(id string) *state.AgentState {
	m.mu.Lock()
	defer m.mu.Unlock()

	a, ok := m.agents[id]
	if !ok {
		return nil
	}
	cp := *a
	return &cp
}

func (m *mockStore) SetAgent(agent *state.AgentState) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	cp := *agent
	m.agents[cp.ID] = &cp
	return nil
}

func (m *mockStore) AllNodes() []*types.NodeState {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := make([]*types.NodeState, 0, len(m.nodes))
	for _, n := range m.nodes {
		cp := *n
		result = append(result, &cp)
	}
	return result
}

// mockVMManager implements VMAccess for testing.
type mockVMManager struct {
	mu      sync.Mutex
	stopped []string
	failFor map[string]error
}

func newMockVMManager() *mockVMManager {
	return &mockVMManager{
		failFor: make(map[string]error),
	}
}

func (m *mockVMManager) StopVM(_ context.Context, agentID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err, ok := m.failFor[agentID]; ok {
		return err
	}
	m.stopped = append(m.stopped, agentID)
	return nil
}

func (m *mockVMManager) stoppedAgents() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]string, len(m.stopped))
	copy(result, m.stopped)
	return result
}

// mockMetrics implements MetricsAccess for testing.
type mockMetrics struct {
	mu     sync.Mutex
	memory map[string]float64
	cpu    map[string]float64
}

func newMockMetrics() *mockMetrics {
	return &mockMetrics{
		memory: make(map[string]float64),
		cpu:    make(map[string]float64),
	}
}

func (m *mockMetrics) SetNodeResourceUsage(nodeID string, memoryPercent, cpuPercent float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.memory[nodeID] = memoryPercent
	m.cpu[nodeID] = cpuPercent
}

// --- Tests ---

func TestCrashRecovery_DetectsDeadProcess(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	store := newMockStore()

	// Agent with a PID that almost certainly does not exist.
	store.agents["agent-dead"] = &state.AgentState{
		ID:     "agent-dead",
		Status: state.AgentStatusRunning,
		VMPID:  999999999, // very unlikely to exist
	}

	// Agent with no PID.
	store.agents["agent-no-pid"] = &state.AgentState{
		ID:     "agent-no-pid",
		Status: state.AgentStatusRunning,
		VMPID:  0,
	}

	// Agent that is stopped (should not be touched).
	store.agents["agent-stopped"] = &state.AgentState{
		ID:     "agent-stopped",
		Status: state.AgentStatusStopped,
	}

	cr := NewCrashRecovery(RecoveryConfig{
		Store:  store,
		Logger: logger,
	})

	if err := cr.Reconcile(); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	// Check that the dead agent was marked as FAILED.
	deadAgent := store.GetAgent("agent-dead")
	if deadAgent == nil {
		t.Fatal("agent-dead not found in store")
	}
	if deadAgent.Status != state.AgentStatusFailed {
		t.Errorf("expected agent-dead status FAILED, got %s", deadAgent.Status)
	}
	if deadAgent.Error != "process not found after crash recovery" {
		t.Errorf("expected error 'process not found after crash recovery', got %q", deadAgent.Error)
	}

	// Check that the no-PID agent was also marked as FAILED.
	noPidAgent := store.GetAgent("agent-no-pid")
	if noPidAgent == nil {
		t.Fatal("agent-no-pid not found in store")
	}
	if noPidAgent.Status != state.AgentStatusFailed {
		t.Errorf("expected agent-no-pid status FAILED, got %s", noPidAgent.Status)
	}

	// Check that the stopped agent was not changed.
	stoppedAgent := store.GetAgent("agent-stopped")
	if stoppedAgent == nil {
		t.Fatal("agent-stopped not found in store")
	}
	if stoppedAgent.Status != state.AgentStatusStopped {
		t.Errorf("expected agent-stopped status STOPPED, got %s", stoppedAgent.Status)
	}
}

func TestRateLimiter_AllowsBurst(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	rl := NewRateLimiter(RateLimiterConfig{
		DefaultRate: 10,
		BurstSize:   5,
		Logger:      logger,
	})

	// All burst messages should be allowed.
	for i := 0; i < 5; i++ {
		if !rl.Allow("test.subject") {
			t.Errorf("message %d within burst should be allowed", i)
		}
	}
}

func TestRateLimiter_ThrottlesExcess(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	rl := NewRateLimiter(RateLimiterConfig{
		DefaultRate: 10,
		BurstSize:   3,
		Logger:      logger,
	})

	// Exhaust the burst.
	for i := 0; i < 3; i++ {
		if !rl.Allow("test.subject") {
			t.Fatalf("message %d within burst should be allowed", i)
		}
	}

	// The next message should be throttled (no time has elapsed for refill).
	if rl.Allow("test.subject") {
		t.Error("message beyond burst should be rejected")
	}

	// After waiting a bit, tokens should refill.
	time.Sleep(200 * time.Millisecond) // At 10/s, 200ms = 2 tokens refilled

	if !rl.Allow("test.subject") {
		t.Error("message after refill should be allowed")
	}
}

func TestRateLimiter_SetRate(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	rl := NewRateLimiter(RateLimiterConfig{
		DefaultRate: 10,
		BurstSize:   2,
		Logger:      logger,
	})

	// Set a very high rate for this subject.
	rl.SetRate("fast.subject", 1000)

	// Exhaust burst.
	for i := 0; i < 2; i++ {
		rl.Allow("fast.subject")
	}

	// With 1000/s rate, even a short sleep should refill tokens.
	time.Sleep(10 * time.Millisecond) // 10ms * 1000/s = 10 tokens

	if !rl.Allow("fast.subject") {
		t.Error("message should be allowed with high rate after brief sleep")
	}
}

func TestRateLimiter_IndependentSubjects(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	rl := NewRateLimiter(RateLimiterConfig{
		DefaultRate: 10,
		BurstSize:   2,
		Logger:      logger,
	})

	// Exhaust burst on subject A.
	for i := 0; i < 2; i++ {
		rl.Allow("subject.a")
	}

	// Subject B should still have its own bucket.
	if !rl.Allow("subject.b") {
		t.Error("subject.b should have its own independent bucket")
	}
}

func TestGracefulShutdown_StopsAllAgents(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	store := newMockStore()
	store.agents["agent-1"] = &state.AgentState{
		ID:     "agent-1",
		Status: state.AgentStatusRunning,
		VMPID:  1234,
	}
	store.agents["agent-2"] = &state.AgentState{
		ID:     "agent-2",
		Status: state.AgentStatusRunning,
		VMPID:  5678,
	}
	store.agents["agent-3"] = &state.AgentState{
		ID:     "agent-3",
		Status: state.AgentStatusStopped,
	}

	vmMgr := newMockVMManager()

	gs := NewGracefulShutdown(ShutdownConfig{
		Store:     store,
		VMManager: vmMgr,
		Logger:    logger,
		Timeout:   5 * time.Second,
	})

	if err := gs.Execute(context.Background()); err != nil {
		t.Fatalf("graceful shutdown failed: %v", err)
	}

	// Verify the running agents were stopped.
	stopped := vmMgr.stoppedAgents()
	if len(stopped) != 2 {
		t.Errorf("expected 2 agents stopped, got %d: %v", len(stopped), stopped)
	}

	// Verify agent states were updated.
	a1 := store.GetAgent("agent-1")
	if a1.Status != state.AgentStatusStopped {
		t.Errorf("expected agent-1 STOPPED, got %s", a1.Status)
	}
	a2 := store.GetAgent("agent-2")
	if a2.Status != state.AgentStatusStopped {
		t.Errorf("expected agent-2 STOPPED, got %s", a2.Status)
	}

	// Verify the already-stopped agent was not touched.
	a3 := store.GetAgent("agent-3")
	if a3.Status != state.AgentStatusStopped {
		t.Errorf("expected agent-3 to remain STOPPED, got %s", a3.Status)
	}
}

func TestGracefulShutdown_HandlesVMFailure(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	store := newMockStore()
	store.agents["agent-fail"] = &state.AgentState{
		ID:     "agent-fail",
		Status: state.AgentStatusRunning,
		VMPID:  1111,
	}

	vmMgr := newMockVMManager()
	vmMgr.failFor["agent-fail"] = fmt.Errorf("vm process hung")

	gs := NewGracefulShutdown(ShutdownConfig{
		Store:     store,
		VMManager: vmMgr,
		Logger:    logger,
		Timeout:   5 * time.Second,
	})

	err := gs.Execute(context.Background())
	if err == nil {
		t.Fatal("expected error from failed VM stop, got nil")
	}

	// Agent should be marked FAILED.
	agent := store.GetAgent("agent-fail")
	if agent.Status != state.AgentStatusFailed {
		t.Errorf("expected agent-fail FAILED, got %s", agent.Status)
	}
}

func TestGracefulShutdown_NoRunningAgents(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	store := newMockStore()
	store.agents["agent-stopped"] = &state.AgentState{
		ID:     "agent-stopped",
		Status: state.AgentStatusStopped,
	}

	vmMgr := newMockVMManager()

	gs := NewGracefulShutdown(ShutdownConfig{
		Store:     store,
		VMManager: vmMgr,
		Logger:    logger,
		Timeout:   5 * time.Second,
	})

	if err := gs.Execute(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(vmMgr.stoppedAgents()) != 0 {
		t.Error("no agents should have been stopped")
	}
}

func TestResourceMonitor_StartsAndStops(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	store := newMockStore()
	metrics := newMockMetrics()

	rm := NewResourceMonitor(MonitorConfig{
		Store:         store,
		Metrics:       metrics,
		Logger:        logger,
		CheckInterval: 50 * time.Millisecond,
	})

	if err := rm.Start(); err != nil {
		t.Fatalf("failed to start resource monitor: %v", err)
	}

	// Let it run a few cycles.
	time.Sleep(200 * time.Millisecond)

	rm.Stop()

	// Stopping twice should be safe.
	rm.Stop()
}

func TestResourceMonitor_RecordsMetrics(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	store := newMockStore()
	store.nodes = []*types.NodeState{
		{
			ID:     "node-1",
			Status: types.NodeStatusOnline,
			Resources: types.NodeResources{
				MemoryTotal: 8 * 1024 * 1024 * 1024, // 8GB
				CPUCount:    4,
			},
			Agents: []string{"agent-1", "agent-2"},
		},
	}

	metrics := newMockMetrics()

	rm := NewResourceMonitor(MonitorConfig{
		Store:         store,
		Metrics:       metrics,
		Logger:        logger,
		CheckInterval: 500 * time.Millisecond,
	})

	if err := rm.Start(); err != nil {
		t.Fatalf("failed to start resource monitor: %v", err)
	}

	// Wait for at least one check cycle. On Linux, CPU measurement takes
	// ~200ms per sample so we need a longer wait.
	time.Sleep(1500 * time.Millisecond)
	rm.Stop()

	// Verify metrics were recorded.
	metrics.mu.Lock()
	defer metrics.mu.Unlock()

	if _, ok := metrics.memory["node-1"]; !ok {
		t.Error("expected memory metrics for node-1")
	}
	if _, ok := metrics.cpu["node-1"]; !ok {
		t.Error("expected CPU metrics for node-1")
	}
}

func TestSystemMemoryUsage_ReturnsNonZero(t *testing.T) {
	percent, total, used, err := systemMemoryUsage()
	if err != nil {
		t.Fatalf("systemMemoryUsage() failed: %v", err)
	}

	if total <= 0 {
		t.Errorf("expected positive total bytes, got %d", total)
	}
	if used < 0 {
		t.Errorf("expected non-negative used bytes, got %d", used)
	}
	if percent < 0 || percent > 100 {
		t.Errorf("expected percent in [0,100], got %.2f", percent)
	}

	t.Logf("memory: %.1f%% used (%d / %d bytes)", percent, used, total)
}

func TestSystemCPUUsage_ReturnsBounded(t *testing.T) {
	percent, err := systemCPUUsage()
	if err != nil {
		t.Fatalf("systemCPUUsage() failed: %v", err)
	}

	if percent < 0 || percent > 100 {
		t.Errorf("expected percent in [0,100], got %.2f", percent)
	}

	t.Logf("cpu: %.1f%%", percent)
}

func TestSystemCPUCount_ReturnsPositive(t *testing.T) {
	count := systemCPUCount()
	if count <= 0 {
		t.Errorf("expected positive CPU count, got %d", count)
	}
	t.Logf("cpu count: %d", count)
}

func TestComputeUsage_LocalNodeUsesRealData(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	rm := NewResourceMonitor(MonitorConfig{
		Logger: logger,
	})

	// Create a node that matches the local system's CPU count so it is
	// detected as the local node.
	localNode := &types.NodeState{
		ID:     "local",
		Status: types.NodeStatusOnline,
		Resources: types.NodeResources{
			MemoryTotal: 16 * 1024 * 1024 * 1024,
			CPUCount:    systemCPUCount(),
		},
		Agents: []string{"agent-1"},
	}

	memPct, cpuPct := rm.computeUsage(localNode)

	// Memory should be non-zero since the Go runtime itself uses memory.
	if memPct <= 0 {
		t.Errorf("expected positive memory percent for local node, got %.2f", memPct)
	}
	// CPU percent should be in valid range (may be 0 on very idle systems).
	if cpuPct < 0 || cpuPct > 100 {
		t.Errorf("expected CPU percent in [0,100], got %.2f", cpuPct)
	}

	t.Logf("local node: mem=%.1f%%, cpu=%.1f%%", memPct, cpuPct)
}

func TestComputeUsage_RemoteNodeUsesEstimate(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	rm := NewResourceMonitor(MonitorConfig{
		Logger: logger,
	})

	// Create a node with a CPU count that will not match the local system
	// (use 9999 CPUs which no real system has).
	remoteNode := &types.NodeState{
		ID:     "remote",
		Status: types.NodeStatusOnline,
		Resources: types.NodeResources{
			MemoryTotal: 8 * 1024 * 1024 * 1024, // 8GB
			CPUCount:    9999,
		},
		Agents: []string{"agent-1", "agent-2"},
	}

	memPct, cpuPct := rm.computeUsage(remoteNode)

	// For a remote node with 2 agents and 8GB, memory estimate should be
	// 2 * 512MB / 8GB = ~12.5%.
	expectedMem := float64(2*512*1024*1024) / float64(8*1024*1024*1024) * 100.0
	if memPct < expectedMem-0.1 || memPct > expectedMem+0.1 {
		t.Errorf("expected memory ~%.1f%%, got %.1f%%", expectedMem, memPct)
	}

	// CPU estimate: 2 agents / 9999 CPUs * 25 = ~0.005%.
	if cpuPct < 0 || cpuPct > 1 {
		t.Errorf("expected very low CPU percent for remote node, got %.4f%%", cpuPct)
	}

	t.Logf("remote node: mem=%.1f%%, cpu=%.4f%%", memPct, cpuPct)
}
