//go:build unit

package scheduler

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivehq/hive/internal/state"
	"github.com/hivehq/hive/internal/types"
)

func newTestStore(t *testing.T) *state.Store {
	t.Helper()
	dir := t.TempDir()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store, err := state.NewStore(filepath.Join(dir, "state.json"), logger)
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	return store
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func addNode(t *testing.T, store *state.Store, id string, memTotal int64, cpuCount int, kvm bool, status types.NodeStatus, labels map[string]string, arch string) {
	t.Helper()
	node := &types.NodeState{
		ID:     id,
		Tier:   types.NodeTier1,
		Arch:   arch,
		Status: status,
		Resources: types.NodeResources{
			MemoryTotal: memTotal,
			CPUCount:    cpuCount,
			KVMAvail:    kvm,
		},
		Labels:   labels,
		JoinedAt: time.Now(),
	}
	if !kvm {
		node.Tier = types.NodeTier2
	}
	if err := store.SetNode(node); err != nil {
		t.Fatalf("setting node %s: %v", id, err)
	}
}

func testManifest(id, team, memory string, vcpus int) *types.AgentManifest {
	return &types.AgentManifest{
		APIVersion: "hive/v1",
		Kind:       "Agent",
		Metadata: types.AgentMetadata{
			ID:   id,
			Team: team,
		},
		Spec: types.AgentSpec{
			Runtime: types.AgentRuntime{Type: "openclaw"},
			Resources: types.AgentResources{
				Memory: memory,
				VCPUs:  vcpus,
			},
		},
	}
}

func TestSchedule_BinPacking(t *testing.T) {
	store := newTestStore(t)

	// Add 3 nodes with varying resources.
	// node-a: 8Gi, 8 CPUs (largest)
	// node-b: 4Gi, 4 CPUs (medium)
	// node-c: 2Gi, 2 CPUs (smallest)
	addNode(t, store, "node-a", 8*1024*1024*1024, 8, true, types.NodeStatusOnline, nil, "amd64")
	addNode(t, store, "node-b", 4*1024*1024*1024, 4, true, types.NodeStatusOnline, nil, "amd64")
	addNode(t, store, "node-c", 2*1024*1024*1024, 2, true, types.NodeStatusOnline, nil, "amd64")

	sched := NewScheduler(store, testLogger())

	// Schedule an agent requiring 1Gi, 1 vCPU.
	manifest := testManifest("agent-1", "", "1Gi", 1)
	assignment, err := sched.Schedule(manifest)
	if err != nil {
		t.Fatalf("Schedule failed: %v", err)
	}

	// All three nodes can fit this agent. The scoring should prefer the node
	// with the most remaining resources after allocation (bin-packing).
	// node-a: (8-1)/8=0.875 mem + (8-1)/8=0.875 cpu = 1.75
	// node-b: (4-1)/4=0.75 mem + (4-1)/4=0.75 cpu = 1.5
	// node-c: (2-1)/2=0.5 mem + (2-1)/2=0.5 cpu = 1.0
	// All are unloaded, so node-a is most loaded (zero load, they tie;
	// alphabetical means node-a gets spread penalty among tied most-loaded).
	// After spread penalty on "node-a": 1.75 - 0.3 = 1.45
	// node-b: 1.5 (no penalty, or also penalized depending on tie)
	// Actually, all have the same load (0), so all are "most loaded" by ratio.
	// The scoring picks the one with alphabetically first nodeID as mostLoaded.
	// node-a gets -0.3, so score = 1.45; node-b = 1.5; node-c = 1.0
	// Best: node-b
	if assignment.NodeID != "node-b" {
		t.Errorf("expected assignment to node-b, got %s", assignment.NodeID)
	}
}

func TestSchedule_PlacementNodeID(t *testing.T) {
	store := newTestStore(t)

	addNode(t, store, "node-a", 8*1024*1024*1024, 8, true, types.NodeStatusOnline, nil, "amd64")
	addNode(t, store, "node-b", 4*1024*1024*1024, 4, true, types.NodeStatusOnline, nil, "amd64")

	sched := NewScheduler(store, testLogger())

	manifest := testManifest("agent-1", "", "1Gi", 1)
	manifest.Spec.Placement.NodeID = "node-b"

	assignment, err := sched.Schedule(manifest)
	if err != nil {
		t.Fatalf("Schedule failed: %v", err)
	}

	if assignment.NodeID != "node-b" {
		t.Errorf("expected placement override to node-b, got %s", assignment.NodeID)
	}
}

func TestSchedule_PlacementNodeLabels(t *testing.T) {
	store := newTestStore(t)

	addNode(t, store, "node-a", 8*1024*1024*1024, 8, true, types.NodeStatusOnline, map[string]string{"zone": "us-west"}, "amd64")
	addNode(t, store, "node-b", 8*1024*1024*1024, 8, true, types.NodeStatusOnline, map[string]string{"zone": "us-east"}, "amd64")

	sched := NewScheduler(store, testLogger())

	manifest := testManifest("agent-1", "", "1Gi", 1)
	manifest.Spec.Placement.NodeLabels = map[string]string{"zone": "us-east"}

	assignment, err := sched.Schedule(manifest)
	if err != nil {
		t.Fatalf("Schedule failed: %v", err)
	}

	if assignment.NodeID != "node-b" {
		t.Errorf("expected label constraint to select node-b, got %s", assignment.NodeID)
	}
}

func TestSchedule_PendingWhenNoNodeFits(t *testing.T) {
	store := newTestStore(t)

	// Add a node with only 256Mi of memory.
	addNode(t, store, "node-a", 256*1024*1024, 2, true, types.NodeStatusOnline, nil, "amd64")

	sched := NewScheduler(store, testLogger())

	// Request 1Gi which exceeds what node-a has.
	manifest := testManifest("agent-1", "", "1Gi", 1)

	_, err := sched.Schedule(manifest)
	if err == nil {
		t.Fatal("expected Schedule to fail when no node fits")
	}
	if got := err.Error(); got == "" {
		t.Error("expected non-empty error message")
	}
}

func TestSchedule_CordonedNodeExcluded(t *testing.T) {
	store := newTestStore(t)

	// node-a is cordoned; node-b is online.
	addNode(t, store, "node-a", 8*1024*1024*1024, 8, true, types.NodeStatusCordoned, nil, "amd64")
	addNode(t, store, "node-b", 4*1024*1024*1024, 4, true, types.NodeStatusOnline, nil, "amd64")

	sched := NewScheduler(store, testLogger())

	manifest := testManifest("agent-1", "", "1Gi", 1)
	assignment, err := sched.Schedule(manifest)
	if err != nil {
		t.Fatalf("Schedule failed: %v", err)
	}

	if assignment.NodeID != "node-b" {
		t.Errorf("expected scheduling to non-cordoned node-b, got %s", assignment.NodeID)
	}
}

func TestSchedule_DrainingNodeExcluded(t *testing.T) {
	store := newTestStore(t)

	addNode(t, store, "node-a", 8*1024*1024*1024, 8, true, types.NodeStatusDraining, nil, "amd64")
	addNode(t, store, "node-b", 4*1024*1024*1024, 4, true, types.NodeStatusOnline, nil, "amd64")

	sched := NewScheduler(store, testLogger())

	manifest := testManifest("agent-1", "", "1Gi", 1)
	assignment, err := sched.Schedule(manifest)
	if err != nil {
		t.Fatalf("Schedule failed: %v", err)
	}

	if assignment.NodeID != "node-b" {
		t.Errorf("expected scheduling to non-draining node-b, got %s", assignment.NodeID)
	}
}

func TestSchedule_TeamColocationBonus(t *testing.T) {
	store := newTestStore(t)

	// Two identical large nodes (16Gi/16CPU). Using large nodes so that the
	// resource cost of the existing agent is minimal relative to the
	// colocation bonus.
	addNode(t, store, "node-a", 16*1024*1024*1024, 16, true, types.NodeStatusOnline, nil, "amd64")
	addNode(t, store, "node-b", 16*1024*1024*1024, 16, true, types.NodeStatusOnline, nil, "amd64")

	// Place a very small existing agent from team "alpha" on node-b.
	// Using minimal resources so the resource score difference is small
	// compared to the colocation bonus (+0.5).
	if err := store.SetAgent(&state.AgentState{
		ID:          "existing-agent",
		Team:        "alpha",
		Status:      state.AgentStatusRunning,
		NodeID:      "node-b",
		MemoryBytes: 256 * 1024 * 1024, // 256Mi
		VCPUs:       1,
	}); err != nil {
		t.Fatalf("setting existing agent: %v", err)
	}

	sched := NewScheduler(store, testLogger())

	// Schedule a new agent on team "alpha". Should prefer node-b due to
	// the team colocation bonus (+0.5).
	manifest := testManifest("agent-2", "alpha", "256Mi", 1)
	assignment, err := sched.Schedule(manifest)
	if err != nil {
		t.Fatalf("Schedule failed: %v", err)
	}

	// node-a (empty):
	//   mem=(16Gi-256Mi)/16Gi ≈ 0.984
	//   cpu=(16-1)/16 = 0.9375
	//   total = 1.922 (no penalties, no bonuses)
	// node-b (256Mi, 1 CPU used):
	//   mem=(16Gi-256Mi-256Mi)/16Gi ≈ 0.969
	//   cpu=(16-1-1)/16 = 0.875
	//   total = 1.844 + 0.5 colocation = 2.344 - 0.3 spread = 2.044
	// node-b (2.044) > node-a (1.922) → node-b wins.
	if assignment.NodeID != "node-b" {
		t.Errorf("expected team colocation to select node-b, got %s", assignment.NodeID)
	}
}

func TestSchedule_ArchMismatch(t *testing.T) {
	store := newTestStore(t)

	addNode(t, store, "node-a", 8*1024*1024*1024, 8, true, types.NodeStatusOnline, nil, "amd64")

	sched := NewScheduler(store, testLogger())

	manifest := testManifest("agent-1", "", "1Gi", 1)
	manifest.Spec.Placement.Arch = "arm64"

	_, err := sched.Schedule(manifest)
	if err == nil {
		t.Fatal("expected Schedule to fail due to arch mismatch")
	}
}

func TestReleaseAgent(t *testing.T) {
	store := newTestStore(t)

	addNode(t, store, "node-a", 8*1024*1024*1024, 8, true, types.NodeStatusOnline, nil, "amd64")

	// Add an agent allocated to node-a.
	if err := store.SetAgent(&state.AgentState{
		ID:          "agent-1",
		Status:      state.AgentStatusRunning,
		NodeID:      "node-a",
		MemoryBytes: 2 * 1024 * 1024 * 1024,
		VCPUs:       2,
	}); err != nil {
		t.Fatalf("setting agent: %v", err)
	}

	sched := NewScheduler(store, testLogger())

	// The scheduler should have rebuilt allocations from store.
	// node-a should have 2Gi and 2 VCPUs allocated.
	// Release the agent.
	if err := sched.ReleaseAgent("agent-1"); err != nil {
		t.Fatalf("ReleaseAgent failed: %v", err)
	}

	// Now schedule a large agent that needs 7Gi; should succeed because
	// the released resources are now available.
	manifest := testManifest("agent-2", "", "7Gi", 6)
	assignment, err := sched.Schedule(manifest)
	if err != nil {
		t.Fatalf("Schedule after release failed: %v", err)
	}
	if assignment.NodeID != "node-a" {
		t.Errorf("expected node-a, got %s", assignment.NodeID)
	}
}
