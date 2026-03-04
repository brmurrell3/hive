//go:build unit

package reconciler

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivehq/hive/internal/state"
)

func newTestStore(t *testing.T, clusterRoot string) *state.Store {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store, err := state.NewStore(filepath.Join(clusterRoot, "state.json"), logger)
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	return store
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// setupClusterRoot creates a minimal cluster root with the given agent manifests.
// agentManifests is a map of agentID -> manifest YAML content.
func setupClusterRoot(t *testing.T, agentManifests map[string]string) string {
	t.Helper()

	dir := t.TempDir()

	// Write cluster.yaml
	clusterYAML := `apiVersion: hive/v1
kind: Cluster
metadata:
  name: test-cluster
spec:
  nats:
    port: 0
    jetstream:
      enabled: true
`
	if err := os.WriteFile(filepath.Join(dir, "cluster.yaml"), []byte(clusterYAML), 0644); err != nil {
		t.Fatalf("writing cluster.yaml: %v", err)
	}

	// Create agents directory and write manifests.
	for id, content := range agentManifests {
		agentDir := filepath.Join(dir, "agents", id)
		if err := os.MkdirAll(agentDir, 0755); err != nil {
			t.Fatalf("creating agent dir %s: %v", id, err)
		}
		if err := os.WriteFile(filepath.Join(agentDir, "manifest.yaml"), []byte(content), 0644); err != nil {
			t.Fatalf("writing manifest for %s: %v", id, err)
		}
	}

	return dir
}

const agentManifestTemplate = `apiVersion: hive/v1
kind: Agent
metadata:
  id: %s
spec:
  runtime:
    type: openclaw
  resources:
    memory: "512Mi"
    vcpus: 1
`

func TestReconcile_CreateAction(t *testing.T) {
	// Agent exists in desired (manifest) but not in actual (store) → create.
	manifests := map[string]string{
		"agent-a": `apiVersion: hive/v1
kind: Agent
metadata:
  id: agent-a
spec:
  runtime:
    type: openclaw
  resources:
    memory: "512Mi"
    vcpus: 1
`,
	}

	clusterRoot := setupClusterRoot(t, manifests)
	store := newTestStore(t, clusterRoot)

	r := NewReconciler(store, clusterRoot, testLogger())
	actions := r.Reconcile()

	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if actions[0].Type != ActionCreate {
		t.Errorf("expected create action, got %s", actions[0].Type)
	}
	if actions[0].AgentID != "agent-a" {
		t.Errorf("expected agent-a, got %s", actions[0].AgentID)
	}
	if actions[0].Manifest == nil {
		t.Error("expected manifest to be set on create action")
	}
}

func TestReconcile_DestroyAction(t *testing.T) {
	// Agent exists in actual (store) but not in desired (no manifest) → destroy.
	clusterRoot := setupClusterRoot(t, nil)
	store := newTestStore(t, clusterRoot)

	// Add an agent to the store that has no corresponding manifest.
	if err := store.SetAgent(&state.AgentState{
		ID:     "agent-orphan",
		Status: state.AgentStatusRunning,
		NodeID: "node-1",
	}); err != nil {
		t.Fatalf("setting agent: %v", err)
	}

	r := NewReconciler(store, clusterRoot, testLogger())
	actions := r.Reconcile()

	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if actions[0].Type != ActionDestroy {
		t.Errorf("expected destroy action, got %s", actions[0].Type)
	}
	if actions[0].AgentID != "agent-orphan" {
		t.Errorf("expected agent-orphan, got %s", actions[0].AgentID)
	}
}

func TestReconcile_NoActionWhenSame(t *testing.T) {
	// Agent exists in both desired and actual with the same manifest hash → no action.
	manifestYAML := `apiVersion: hive/v1
kind: Agent
metadata:
  id: agent-stable
spec:
  runtime:
    type: openclaw
  resources:
    memory: "512Mi"
    vcpus: 1
`
	manifests := map[string]string{
		"agent-stable": manifestYAML,
	}

	clusterRoot := setupClusterRoot(t, manifests)
	store := newTestStore(t, clusterRoot)

	// Load the desired state to compute the manifest hash.
	// We need to parse the manifest the same way the reconciler does.
	desiredManifest := manifests["agent-stable"]
	_ = desiredManifest

	// Compute the expected hash by loading the manifest through config.
	// The reconciler uses config.LoadDesiredState, so we do the same.
	r := NewReconciler(store, clusterRoot, testLogger())

	// First reconcile should produce a create action (agent not in store).
	actions := r.Reconcile()
	if len(actions) != 1 {
		t.Fatalf("expected 1 create action on first reconcile, got %d", len(actions))
	}
	if actions[0].Type != ActionCreate {
		t.Fatalf("expected create action, got %s", actions[0].Type)
	}

	// Simulate the agent being created: add it to the store with the correct
	// manifest hash.
	hash := ManifestHash(actions[0].Manifest)
	if err := store.SetAgent(&state.AgentState{
		ID:           "agent-stable",
		Status:       state.AgentStatusRunning,
		ManifestHash: hash,
	}); err != nil {
		t.Fatalf("setting agent: %v", err)
	}

	// Second reconcile should produce no actions.
	actions = r.Reconcile()
	if len(actions) != 0 {
		t.Errorf("expected 0 actions when manifest unchanged, got %d: %+v", len(actions), actions)
	}
}

func TestReconcile_RestartOnManifestChange(t *testing.T) {
	// Agent exists in both desired and actual, but manifest hash differs → restart.
	manifestYAML := `apiVersion: hive/v1
kind: Agent
metadata:
  id: agent-changed
spec:
  runtime:
    type: openclaw
  resources:
    memory: "512Mi"
    vcpus: 1
`
	manifests := map[string]string{
		"agent-changed": manifestYAML,
	}

	clusterRoot := setupClusterRoot(t, manifests)
	store := newTestStore(t, clusterRoot)

	// Add the agent to the store with a different (old) manifest hash.
	if err := store.SetAgent(&state.AgentState{
		ID:           "agent-changed",
		Status:       state.AgentStatusRunning,
		ManifestHash: "old-hash-that-does-not-match",
		NodeID:       "node-1",
	}); err != nil {
		t.Fatalf("setting agent: %v", err)
	}

	r := NewReconciler(store, clusterRoot, testLogger())
	actions := r.Reconcile()

	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if actions[0].Type != ActionRestart {
		t.Errorf("expected restart action, got %s", actions[0].Type)
	}
	if actions[0].AgentID != "agent-changed" {
		t.Errorf("expected agent-changed, got %s", actions[0].AgentID)
	}
	if actions[0].Manifest == nil {
		t.Error("expected manifest to be set on restart action")
	}
}

func TestReconcile_MultipleActions(t *testing.T) {
	// Test ordering: destroy first, then create, then restart.
	manifestYAML := `apiVersion: hive/v1
kind: Agent
metadata:
  id: agent-new
spec:
  runtime:
    type: openclaw
  resources:
    memory: "512Mi"
    vcpus: 1
`
	changedYAML := `apiVersion: hive/v1
kind: Agent
metadata:
  id: agent-existing
spec:
  runtime:
    type: openclaw
  resources:
    memory: "1Gi"
    vcpus: 2
`
	manifests := map[string]string{
		"agent-new":      manifestYAML,
		"agent-existing": changedYAML,
	}

	clusterRoot := setupClusterRoot(t, manifests)
	store := newTestStore(t, clusterRoot)

	// Add an orphaned agent (will be destroyed).
	if err := store.SetAgent(&state.AgentState{
		ID:     "agent-orphan",
		Status: state.AgentStatusRunning,
		NodeID: "node-1",
	}); err != nil {
		t.Fatalf("setting orphan agent: %v", err)
	}

	// Add agent-existing with a stale hash (will be restarted).
	if err := store.SetAgent(&state.AgentState{
		ID:           "agent-existing",
		Status:       state.AgentStatusRunning,
		ManifestHash: "stale-hash",
		NodeID:       "node-1",
	}); err != nil {
		t.Fatalf("setting existing agent: %v", err)
	}

	r := NewReconciler(store, clusterRoot, testLogger())
	actions := r.Reconcile()

	// Expect 3 actions: 1 destroy, 1 create, 1 restart.
	if len(actions) != 3 {
		t.Fatalf("expected 3 actions, got %d: %+v", len(actions), actions)
	}

	// Verify ordering: destroy first, then create, then restart.
	if actions[0].Type != ActionDestroy {
		t.Errorf("action[0] expected destroy, got %s", actions[0].Type)
	}
	if actions[0].AgentID != "agent-orphan" {
		t.Errorf("action[0] expected agent-orphan, got %s", actions[0].AgentID)
	}

	if actions[1].Type != ActionCreate {
		t.Errorf("action[1] expected create, got %s", actions[1].Type)
	}
	if actions[1].AgentID != "agent-new" {
		t.Errorf("action[1] expected agent-new, got %s", actions[1].AgentID)
	}

	if actions[2].Type != ActionRestart {
		t.Errorf("action[2] expected restart, got %s", actions[2].Type)
	}
	if actions[2].AgentID != "agent-existing" {
		t.Errorf("action[2] expected agent-existing, got %s", actions[2].AgentID)
	}
}

func TestReconcile_EmptyManifestHash(t *testing.T) {
	// When an agent has an empty manifest hash (e.g., pre-M8 agent), no
	// restart action should be generated even if the hash would differ.
	manifestYAML := `apiVersion: hive/v1
kind: Agent
metadata:
  id: agent-legacy
spec:
  runtime:
    type: openclaw
  resources:
    memory: "512Mi"
    vcpus: 1
`
	manifests := map[string]string{
		"agent-legacy": manifestYAML,
	}

	clusterRoot := setupClusterRoot(t, manifests)
	store := newTestStore(t, clusterRoot)

	// Agent exists with empty ManifestHash (pre-M8 behavior).
	if err := store.SetAgent(&state.AgentState{
		ID:           "agent-legacy",
		Status:       state.AgentStatusRunning,
		ManifestHash: "", // empty = legacy, no restart
	}); err != nil {
		t.Fatalf("setting agent: %v", err)
	}

	r := NewReconciler(store, clusterRoot, testLogger())
	actions := r.Reconcile()

	if len(actions) != 0 {
		t.Errorf("expected 0 actions for empty manifest hash, got %d: %+v", len(actions), actions)
	}
}
