// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build unit

package reconciler

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/brmurrell3/hive/internal/state"
)

func newTestStore(t *testing.T, clusterRoot string) *state.Store {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store, err := state.NewStore(filepath.Join(clusterRoot, "state.db"), logger)
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
	// Agent exists in actual (store) but not in desired → destroy.
	// We include at least one desired agent so the mass-destruction safeguard
	// (which blocks destroy when desired is empty) does not trigger.
	keepManifest := `apiVersion: hive/v1
kind: Agent
metadata:
  id: agent-keep
spec:
  runtime:
    type: openclaw
  resources:
    memory: "512Mi"
    vcpus: 1
`
	manifests := map[string]string{
		"agent-keep": keepManifest,
	}
	clusterRoot := setupClusterRoot(t, manifests)
	store := newTestStore(t, clusterRoot)

	// Add an orphan agent (no manifest) and additional agents so the
	// mass-destruction safeguard (>50%) does not trigger.
	for _, id := range []string{"agent-orphan", "agent-keep"} {
		if err := store.SetAgent(&state.AgentState{
			ID:     id,
			Status: state.AgentStatusRunning,
			NodeID: "node-1",
		}); err != nil {
			t.Fatalf("setting agent %s: %v", id, err)
		}
	}

	r := NewReconciler(store, clusterRoot, testLogger())
	actions := r.Reconcile()

	// Expect destroy agent-orphan (agent-keep matches desired state).
	var destroyCount int
	for _, a := range actions {
		if a.Type == ActionDestroy && a.AgentID == "agent-orphan" {
			destroyCount++
		}
	}
	if destroyCount != 1 {
		t.Errorf("expected 1 destroy action for agent-orphan, got %d (actions: %+v)", destroyCount, actions)
	}
}

func TestReconcile_MassDestructionSafeguard(t *testing.T) {
	// When desired state has 0 agents but the store has running agents,
	// the reconciler should skip all actions to prevent mass destruction.
	clusterRoot := setupClusterRoot(t, nil)
	store := newTestStore(t, clusterRoot)

	if err := store.SetAgent(&state.AgentState{
		ID:     "agent-orphan",
		Status: state.AgentStatusRunning,
		NodeID: "node-1",
	}); err != nil {
		t.Fatalf("setting agent: %v", err)
	}

	r := NewReconciler(store, clusterRoot, testLogger())
	actions := r.Reconcile()

	if len(actions) != 0 {
		t.Errorf("expected 0 actions due to mass destruction safeguard, got %d: %+v", len(actions), actions)
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
	hash, err := ManifestHash(actions[0].Manifest)
	if err != nil {
		t.Fatalf("ManifestHash: %v", err)
	}
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

func TestReconcile_ExternalAgentSkipped(t *testing.T) {
	// An external agent (native tier, no command, no mode set) should not
	// generate create or restart actions — it joins via hive-agent.
	manifests := map[string]string{
		"ext-agent": `apiVersion: hive/v1
kind: Agent
metadata:
  id: ext-agent
spec:
  tier: native
  runtime:
    type: openclaw
`,
	}

	clusterRoot := setupClusterRoot(t, manifests)
	store := newTestStore(t, clusterRoot)

	r := NewReconciler(store, clusterRoot, testLogger())
	actions := r.Reconcile()

	// External agent should be skipped — no create action.
	if len(actions) != 0 {
		t.Errorf("expected 0 actions for external agent, got %d: %+v", len(actions), actions)
	}
}

func TestReconcile_ExternalAgentExplicitMode(t *testing.T) {
	// An agent with explicit mode: external should be skipped even if it
	// has a runtime command set.
	manifests := map[string]string{
		"ext-explicit": `apiVersion: hive/v1
kind: Agent
metadata:
  id: ext-explicit
spec:
  tier: native
  mode: external
  runtime:
    type: custom
    command: /usr/bin/my-agent
`,
	}

	clusterRoot := setupClusterRoot(t, manifests)
	store := newTestStore(t, clusterRoot)

	r := NewReconciler(store, clusterRoot, testLogger())
	actions := r.Reconcile()

	if len(actions) != 0 {
		t.Errorf("expected 0 actions for explicit external agent, got %d: %+v", len(actions), actions)
	}
}

func TestReconcile_ManagedNativeAgentCreated(t *testing.T) {
	// A managed native agent (has runtime command) should be created.
	manifests := map[string]string{
		"managed-native": `apiVersion: hive/v1
kind: Agent
metadata:
  id: managed-native
spec:
  tier: native
  runtime:
    type: custom
    command: /usr/bin/my-agent
`,
	}

	clusterRoot := setupClusterRoot(t, manifests)
	store := newTestStore(t, clusterRoot)

	r := NewReconciler(store, clusterRoot, testLogger())
	actions := r.Reconcile()

	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d: %+v", len(actions), actions)
	}
	if actions[0].Type != ActionCreate {
		t.Errorf("expected create action, got %s", actions[0].Type)
	}
}

func TestReconcile_ExternalAgentNotRestarted(t *testing.T) {
	// An external agent that exists in both desired and actual should
	// not be restarted, even if it has a stale manifest hash.
	manifests := map[string]string{
		"ext-agent": `apiVersion: hive/v1
kind: Agent
metadata:
  id: ext-agent
spec:
  tier: native
  runtime:
    type: openclaw
`,
	}

	clusterRoot := setupClusterRoot(t, manifests)
	store := newTestStore(t, clusterRoot)

	if err := store.SetAgent(&state.AgentState{
		ID:           "ext-agent",
		Status:       state.AgentStatusRunning,
		ManifestHash: "stale-hash",
	}); err != nil {
		t.Fatalf("setting agent: %v", err)
	}

	r := NewReconciler(store, clusterRoot, testLogger())
	actions := r.Reconcile()

	if len(actions) != 0 {
		t.Errorf("expected 0 actions for external agent, got %d: %+v", len(actions), actions)
	}
}

func TestReconcile_BackwardCompatManagedVM(t *testing.T) {
	// VM agents without explicit mode should still be created (backward compat).
	manifests := map[string]string{
		"vm-agent": `apiVersion: hive/v1
kind: Agent
metadata:
  id: vm-agent
spec:
  tier: vm
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
		t.Fatalf("expected 1 action for vm agent, got %d: %+v", len(actions), actions)
	}
	if actions[0].Type != ActionCreate {
		t.Errorf("expected create, got %s", actions[0].Type)
	}
}
