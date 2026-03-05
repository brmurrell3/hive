// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build unit

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brmurrell3/hive/internal/types"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func validAgent(id, team string) *types.AgentManifest {
	return &types.AgentManifest{
		APIVersion: "hive/v1",
		Kind:       "Agent",
		Metadata:   types.AgentMetadata{ID: id, Team: team},
		Spec: types.AgentSpec{
			Runtime: types.AgentRuntime{Type: "openclaw"},
		},
	}
}

func validTeam(id, lead string) *types.TeamManifest {
	return &types.TeamManifest{
		APIVersion: "hive/v1",
		Kind:       "Team",
		Metadata:   types.TeamMetadata{ID: id},
		Spec:       types.TeamSpec{Lead: lead},
	}
}

func validCluster() *types.ClusterConfig {
	return &types.ClusterConfig{
		APIVersion: "hive/v1",
		Kind:       "Cluster",
		Metadata:   types.ClusterMetadata{Name: "test"},
		Spec: types.ClusterSpec{
			Defaults: types.DefaultsConfig{
				Restart: types.RestartConfig{Policy: "on-failure"},
			},
		},
	}
}

// mustDS builds a DesiredState from the supplied parts. Nil cluster gets a
// default. Nil maps are initialised to empty maps.
func mustDS(t *testing.T, cluster *types.ClusterConfig, agents map[string]*types.AgentManifest, teams map[string]*types.TeamManifest) *types.DesiredState {
	t.Helper()
	if cluster == nil {
		cluster = validCluster()
	}
	if agents == nil {
		agents = map[string]*types.AgentManifest{}
	}
	if teams == nil {
		teams = map[string]*types.TeamManifest{}
	}
	return &types.DesiredState{
		Cluster: cluster,
		Agents:  agents,
		Teams:   teams,
	}
}

// requireNoError fails immediately if err is non-nil.
func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// requireError fails immediately if err is nil.
func requireError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error but got nil")
	}
}

// requireValidationError asserts err is a *ValidationError and returns it.
func requireValidationError(t *testing.T, err error) *ValidationError {
	t.Helper()
	requireError(t, err)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected *ValidationError, got %T: %v", err, err)
	}
	return ve
}

// assertErrorMentions checks that at least one error string in the
// ValidationError contains every one of the given substrings.
func assertErrorMentions(t *testing.T, ve *ValidationError, substrings ...string) {
	t.Helper()
	for _, sub := range substrings {
		found := false
		for _, e := range ve.Errors {
			if strings.Contains(e, sub) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected at least one validation error containing %q; errors: %v", sub, ve.Errors)
		}
	}
}

// ---------------------------------------------------------------------------
// AC-1: Valid manifests parse without error, all fields populated correctly
// ---------------------------------------------------------------------------

func TestValidateDesiredState_ValidManifests(t *testing.T) {
	t.Parallel()
	agent := validAgent("my-agent", "backend")
	agent.Spec.Resources = types.AgentResources{Memory: "512Mi", VCPUs: 2}
	agent.Spec.Capabilities = []types.AgentCapability{
		{Name: "summarise", Description: "Summarises text"},
	}

	team := validTeam("backend", "my-agent")

	ds := mustDS(t, nil,
		map[string]*types.AgentManifest{"my-agent": agent},
		map[string]*types.TeamManifest{"backend": team},
	)

	err := ValidateDesiredState(ds)
	requireNoError(t, err)
}

func TestValidateDesiredState_AllFieldsPopulated(t *testing.T) {
	t.Parallel()
	agent := validAgent("worker1", "ops")
	agent.Spec.Resources = types.AgentResources{Memory: "1Gi", VCPUs: 4, Disk: "10Gi"}
	agent.Spec.Capabilities = []types.AgentCapability{
		{
			Name:        "deploy",
			Description: "Deploys an application",
			Inputs:      []types.CapabilityParam{{Name: "app", Type: "string"}},
			Outputs:     []types.CapabilityParam{{Name: "status", Type: "string"}},
			Async:       true,
		},
	}
	agent.Spec.Restart = types.AgentRestart{Policy: "on-failure", MaxRestarts: 3}

	team := validTeam("ops", "worker1")
	team.Spec.SharedVolumes = []types.SharedVolume{
		{Name: "data", HostPath: "/mnt/data", Access: "read-write"},
	}

	ds := mustDS(t, nil,
		map[string]*types.AgentManifest{"worker1": agent},
		map[string]*types.TeamManifest{"ops": team},
	)

	err := ValidateDesiredState(ds)
	requireNoError(t, err)
}

// ---------------------------------------------------------------------------
// AC-2: Missing metadata.id returns error mentioning the field name
// ---------------------------------------------------------------------------

func TestValidateDesiredState_MissingAgentMetadataID(t *testing.T) {
	t.Parallel()
	agent := validAgent("", "")
	ds := mustDS(t, nil,
		map[string]*types.AgentManifest{"": agent},
		nil,
	)

	ve := requireValidationError(t, ValidateDesiredState(ds))
	assertErrorMentions(t, ve, "metadata.id")
}

func TestValidateDesiredState_MissingTeamMetadataID(t *testing.T) {
	t.Parallel()
	team := validTeam("", "")
	ds := mustDS(t, nil,
		nil,
		map[string]*types.TeamManifest{"": team},
	)

	ve := requireValidationError(t, ValidateDesiredState(ds))
	assertErrorMentions(t, ve, "metadata.id")
}

// ---------------------------------------------------------------------------
// AC-3: Agent ID validation — regex [a-z0-9][a-z0-9-]{0,62}
// ---------------------------------------------------------------------------

func TestValidateDesiredState_AgentIDPatterns(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		id      string
		wantErr bool
	}{
		{name: "valid lowercase with dash", id: "my-agent", wantErr: false},
		{name: "leading dash fails", id: "-leading-dash", wantErr: true},
		{name: "uppercase fails", id: "MyAgent", wantErr: true},
		{name: "64 chars fails", id: strings.Repeat("a", 64), wantErr: true},
		{name: "63 chars passes", id: strings.Repeat("a", 63), wantErr: false},
		{name: "empty string fails", id: "", wantErr: true},
		{name: "single char passes", id: "a", wantErr: false},
		{name: "numeric start passes", id: "0agent", wantErr: false},
		{name: "trailing dash passes", id: "agent-", wantErr: false},
		{name: "underscores fail", id: "my_agent", wantErr: true},
		{name: "dots fail", id: "my.agent", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			agent := validAgent(tt.id, "")
			ds := mustDS(t, nil,
				map[string]*types.AgentManifest{tt.id: agent},
				nil,
			)

			err := ValidateDesiredState(ds)
			if tt.wantErr {
				requireError(t, err)
			} else {
				requireNoError(t, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// AC-4: Duplicate agent IDs across two manifests (detected at load/validate)
//
// The loader stores agents in a map keyed by ID, so a second agent with the
// same ID silently overwrites the first in-memory. This test verifies that
// the map-based representation correctly deduplicates (only one entry for the
// key), and then demonstrates the scenario where two distinct manifests with
// the same metadata.id are present — only the last one survives in the map.
// ---------------------------------------------------------------------------

func TestValidateDesiredState_DuplicateAgentIDs_MapOverwrite(t *testing.T) {
	t.Parallel()
	// Simulate what the loader does: two files, same metadata.id.
	// The second one wins — the map will only have one entry.
	agent1 := validAgent("dup-agent", "")
	agent1.Spec.Resources.Memory = "256Mi"

	agent2 := validAgent("dup-agent", "")
	agent2.Spec.Resources.Memory = "1Gi"

	agents := map[string]*types.AgentManifest{}
	agents["dup-agent"] = agent1
	agents["dup-agent"] = agent2 // overwrites

	if len(agents) != 1 {
		t.Fatalf("expected map to have 1 entry, got %d", len(agents))
	}
	if agents["dup-agent"].Spec.Resources.Memory != "1Gi" {
		t.Fatalf("expected last-writer-wins; got memory=%s", agents["dup-agent"].Spec.Resources.Memory)
	}

	// The surviving state is still valid.
	ds := mustDS(t, nil, agents, nil)
	requireNoError(t, ValidateDesiredState(ds))
}

func TestLoadAgents_DuplicateAgentIDs_OnDisk(t *testing.T) {
	t.Parallel()
	// Two directories, both producing the same metadata.id.
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, "agents")

	// Create two agent dirs whose manifest.yaml both have id: dup-agent
	for _, dirname := range []string{"dup-agent-a", "dup-agent-b"} {
		agentDir := filepath.Join(agentsDir, dirname)
		if err := os.MkdirAll(agentDir, 0755); err != nil {
			t.Fatal(err)
		}
		manifest := `apiVersion: hive/v1
kind: Agent
metadata:
  id: dup-agent
spec:
  runtime:
    type: openclaw
`
		if err := os.WriteFile(filepath.Join(agentDir, "manifest.yaml"), []byte(manifest), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Duplicate agent IDs should now return an error.
	agents, err := LoadAgents(dir)
	if err == nil {
		t.Fatalf("expected error for duplicate agent IDs, got %d agents", len(agents))
	}
	if !strings.Contains(err.Error(), "duplicate agent ID") {
		t.Errorf("expected 'duplicate agent ID' in error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// AC-5: Memory parsing — "512Mi" -> 536870912, "invalid" -> error
// ---------------------------------------------------------------------------

func TestParseMemory(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   string
		want    int64
		wantErr bool
	}{
		{name: "512Mi", input: "512Mi", want: 536870912, wantErr: false},
		{name: "1Gi", input: "1Gi", want: 1073741824, wantErr: false},
		{name: "256MB", input: "256MB", want: 256000000, wantErr: false},
		{name: "1GB", input: "1GB", want: 1000000000, wantErr: false},
		{name: "1024Ki", input: "1024Ki", want: 1048576, wantErr: false},
		{name: "plain bytes", input: "1024", want: 1024, wantErr: false},
		{name: "bytes with B", input: "1024B", want: 1024, wantErr: false},
		{name: "1M", input: "1M", want: 1048576, wantErr: false},
		{name: "2G", input: "2G", want: 2147483648, wantErr: false},
		{name: "invalid string", input: "invalid", want: 0, wantErr: true},
		{name: "empty string", input: "", want: 0, wantErr: true},
		{name: "bad suffix", input: "512Ti", want: 0, wantErr: true},
		{name: "negative would be no digits before suffix", input: "-1Mi", want: 0, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseMemory(tt.input)
			if tt.wantErr {
				requireError(t, err)
				return
			}
			requireNoError(t, err)
			if got != tt.want {
				t.Errorf("ParseMemory(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestValidateDesiredState_InvalidMemory(t *testing.T) {
	t.Parallel()
	agent := validAgent("mem-agent", "")
	agent.Spec.Resources.Memory = "invalid"

	ds := mustDS(t, nil,
		map[string]*types.AgentManifest{"mem-agent": agent},
		nil,
	)

	ve := requireValidationError(t, ValidateDesiredState(ds))
	assertErrorMentions(t, ve, "spec.resources.memory", "invalid")
}

func TestValidateDesiredState_ValidMemory(t *testing.T) {
	t.Parallel()
	agent := validAgent("mem-agent", "")
	agent.Spec.Resources.Memory = "512Mi"

	ds := mustDS(t, nil,
		map[string]*types.AgentManifest{"mem-agent": agent},
		nil,
	)

	requireNoError(t, ValidateDesiredState(ds))
}

// ---------------------------------------------------------------------------
// AC-6: VCPUs — 0 passes (optional), -1 fails
// ---------------------------------------------------------------------------

func TestValidateDesiredState_VCPUs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		vcpus   int
		wantErr bool
	}{
		{name: "zero is optional", vcpus: 0, wantErr: false},
		{name: "positive value", vcpus: 4, wantErr: false},
		{name: "negative fails", vcpus: -1, wantErr: true},
		{name: "large negative fails", vcpus: -100, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			agent := validAgent("cpu-agent", "")
			agent.Spec.Resources.VCPUs = tt.vcpus

			ds := mustDS(t, nil,
				map[string]*types.AgentManifest{"cpu-agent": agent},
				nil,
			)

			err := ValidateDesiredState(ds)
			if tt.wantErr {
				ve := requireValidationError(t, err)
				assertErrorMentions(t, ve, "spec.resources.vcpus")
			} else {
				requireNoError(t, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// AC-7: Team lead references agent ID not present in any manifest: error
// ---------------------------------------------------------------------------

func TestValidateDesiredState_TeamLeadReferencesNonexistentAgent(t *testing.T) {
	t.Parallel()
	team := validTeam("alpha", "ghost-agent")

	ds := mustDS(t, nil,
		map[string]*types.AgentManifest{}, // no agents at all
		map[string]*types.TeamManifest{"alpha": team},
	)

	ve := requireValidationError(t, ValidateDesiredState(ds))
	assertErrorMentions(t, ve, "lead", "ghost-agent", "nonexistent")
}

func TestValidateDesiredState_TeamLeadReferencesExistingAgent(t *testing.T) {
	t.Parallel()
	agent := validAgent("real-agent", "alpha")
	team := validTeam("alpha", "real-agent")

	ds := mustDS(t, nil,
		map[string]*types.AgentManifest{"real-agent": agent},
		map[string]*types.TeamManifest{"alpha": team},
	)

	requireNoError(t, ValidateDesiredState(ds))
}

// ---------------------------------------------------------------------------
// AC-8: Team lead references agent whose metadata.team does not match
// ---------------------------------------------------------------------------

func TestValidateDesiredState_TeamLeadWrongTeam(t *testing.T) {
	t.Parallel()
	agent := validAgent("worker", "other-team") // belongs to "other-team"
	team := validTeam("alpha", "worker")        // alpha says lead is "worker"

	ds := mustDS(t, nil,
		map[string]*types.AgentManifest{"worker": agent},
		map[string]*types.TeamManifest{
			"alpha":      team,
			"other-team": validTeam("other-team", ""),
		},
	)

	ve := requireValidationError(t, ValidateDesiredState(ds))
	assertErrorMentions(t, ve, "lead", "worker", "metadata.team")
}

func TestValidateDesiredState_TeamLeadMatchingTeam(t *testing.T) {
	t.Parallel()
	agent := validAgent("worker", "alpha")
	team := validTeam("alpha", "worker")

	ds := mustDS(t, nil,
		map[string]*types.AgentManifest{"worker": agent},
		map[string]*types.TeamManifest{"alpha": team},
	)

	requireNoError(t, ValidateDesiredState(ds))
}

// ---------------------------------------------------------------------------
// AC-9: Agent volume references shared_volumes name not defined in team
// ---------------------------------------------------------------------------

func TestValidateDesiredState_VolumeReferencesUndefinedSharedVolume(t *testing.T) {
	t.Parallel()
	agent := validAgent("vol-agent", "alpha")
	agent.Spec.Tier = "vm"
	agent.Spec.Volumes = []types.AgentVolume{
		{Name: "nonexistent-vol", MountPath: "/data"},
	}

	team := validTeam("alpha", "")
	team.Spec.SharedVolumes = []types.SharedVolume{
		{Name: "actual-vol", HostPath: "/mnt/actual"},
	}

	ds := mustDS(t, nil,
		map[string]*types.AgentManifest{"vol-agent": agent},
		map[string]*types.TeamManifest{"alpha": team},
	)

	ve := requireValidationError(t, ValidateDesiredState(ds))
	assertErrorMentions(t, ve, "volume", "nonexistent-vol", "shared_volume")
}

func TestValidateDesiredState_VolumeReferencesValidSharedVolume(t *testing.T) {
	t.Parallel()
	agent := validAgent("vol-agent", "alpha")
	agent.Spec.Tier = "vm"
	agent.Spec.Volumes = []types.AgentVolume{
		{Name: "shared-data", MountPath: "/data"},
	}

	team := validTeam("alpha", "")
	team.Spec.SharedVolumes = []types.SharedVolume{
		{Name: "shared-data", HostPath: "/mnt/data"},
	}

	ds := mustDS(t, nil,
		map[string]*types.AgentManifest{"vol-agent": agent},
		map[string]*types.TeamManifest{"alpha": team},
	)

	requireNoError(t, ValidateDesiredState(ds))
}

// ---------------------------------------------------------------------------
// AC-10: Duplicate capability names within one agent
// ---------------------------------------------------------------------------

func TestValidateDesiredState_DuplicateCapabilityNames(t *testing.T) {
	t.Parallel()
	agent := validAgent("cap-agent", "")
	agent.Spec.Capabilities = []types.AgentCapability{
		{Name: "search", Description: "Search the web"},
		{Name: "search", Description: "Search something else"},
	}

	ds := mustDS(t, nil,
		map[string]*types.AgentManifest{"cap-agent": agent},
		nil,
	)

	ve := requireValidationError(t, ValidateDesiredState(ds))
	assertErrorMentions(t, ve, "duplicate capability", "search")
}

func TestValidateDesiredState_UniqueCapabilityNames(t *testing.T) {
	t.Parallel()
	agent := validAgent("cap-agent", "")
	agent.Spec.Capabilities = []types.AgentCapability{
		{Name: "search", Description: "Search the web"},
		{Name: "summarise", Description: "Summarise text"},
	}

	ds := mustDS(t, nil,
		map[string]*types.AgentManifest{"cap-agent": agent},
		nil,
	)

	requireNoError(t, ValidateDesiredState(ds))
}

// ---------------------------------------------------------------------------
// AC-11: Capability with missing name or description
// ---------------------------------------------------------------------------

func TestValidateDesiredState_CapabilityMissingName(t *testing.T) {
	t.Parallel()
	agent := validAgent("cap-agent", "")
	agent.Spec.Capabilities = []types.AgentCapability{
		{Name: "", Description: "A capability without a name"},
	}

	ds := mustDS(t, nil,
		map[string]*types.AgentManifest{"cap-agent": agent},
		nil,
	)

	ve := requireValidationError(t, ValidateDesiredState(ds))
	assertErrorMentions(t, ve, "capability name is required")
}

func TestValidateDesiredState_CapabilityMissingDescription(t *testing.T) {
	t.Parallel()
	agent := validAgent("cap-agent", "")
	agent.Spec.Capabilities = []types.AgentCapability{
		{Name: "search", Description: ""},
	}

	ds := mustDS(t, nil,
		map[string]*types.AgentManifest{"cap-agent": agent},
		nil,
	)

	ve := requireValidationError(t, ValidateDesiredState(ds))
	assertErrorMentions(t, ve, "description is required")
}

func TestValidateDesiredState_CapabilityMissingBothNameAndDescription(t *testing.T) {
	t.Parallel()
	agent := validAgent("cap-agent", "")
	agent.Spec.Capabilities = []types.AgentCapability{
		{Name: "", Description: ""},
	}

	ds := mustDS(t, nil,
		map[string]*types.AgentManifest{"cap-agent": agent},
		nil,
	)

	ve := requireValidationError(t, ValidateDesiredState(ds))
	assertErrorMentions(t, ve, "capability name is required")
}

// ---------------------------------------------------------------------------
// AC-12: hivectl init output passes hivectl validate (config level)
//
// We scaffold the init templates on disk, load them, and validate the
// resulting DesiredState — no errors expected.
// ---------------------------------------------------------------------------

func TestInitOutputPassesValidation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Reproduce the scaffolding that `hivectl init` performs.
	clusterYAML := `apiVersion: hive/v1
kind: Cluster
metadata:
  name: my-cluster
spec:
  nats:
    port: 4222
    jetstream:
      enabled: true
  defaults:
    resources:
      memory: "512Mi"
      vcpus: 2
    health:
      interval: "30s"
      timeout: "5s"
      maxFailures: 3
    restart:
      policy: on-failure
      maxRestarts: 5
      backoff: "10s"
`
	agentYAML := `apiVersion: hive/v1
kind: Agent
metadata:
  id: example-agent
  team: default
spec:
  runtime:
    type: openclaw
    model:
      provider: anthropic
      name: claude-sonnet-4-5
  capabilities:
    - name: answer-questions
      description: Answers general knowledge questions
      inputs:
        - name: question
          type: string
          description: The question to answer
      outputs:
        - name: answer
          type: string
          description: The answer
  resources:
    memory: "512Mi"
    vcpus: 2
`
	teamYAML := `apiVersion: hive/v1
kind: Team
metadata:
  id: default
spec:
  lead: example-agent
`

	// Create directory structure.
	agentDir := filepath.Join(dir, "agents", "example-agent")
	teamsDir := filepath.Join(dir, "teams")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(teamsDir, 0755); err != nil {
		t.Fatal(err)
	}

	writeInitFile(t, filepath.Join(dir, "cluster.yaml"), clusterYAML)
	writeInitFile(t, filepath.Join(agentDir, "manifest.yaml"), agentYAML)
	writeInitFile(t, filepath.Join(teamsDir, "default.yaml"), teamYAML)

	// Load the desired state the same way hivectl validate does.
	ds, err := LoadDesiredState(dir)
	requireNoError(t, err)

	// Validate.
	err = ValidateDesiredState(ds)
	requireNoError(t, err)
}

func writeInitFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// ---------------------------------------------------------------------------
// Additional validation coverage — apiVersion / kind / runtime
// ---------------------------------------------------------------------------

func TestValidateDesiredState_AgentWrongAPIVersion(t *testing.T) {
	t.Parallel()
	agent := validAgent("bad-api", "")
	agent.APIVersion = "hive/v2"

	ds := mustDS(t, nil,
		map[string]*types.AgentManifest{"bad-api": agent},
		nil,
	)

	ve := requireValidationError(t, ValidateDesiredState(ds))
	assertErrorMentions(t, ve, "apiVersion")
}

func TestValidateDesiredState_AgentWrongKind(t *testing.T) {
	t.Parallel()
	agent := validAgent("bad-kind", "")
	agent.Kind = "Team"

	ds := mustDS(t, nil,
		map[string]*types.AgentManifest{"bad-kind": agent},
		nil,
	)

	ve := requireValidationError(t, ValidateDesiredState(ds))
	assertErrorMentions(t, ve, "kind")
}

func TestValidateDesiredState_AgentMissingRuntimeType(t *testing.T) {
	t.Parallel()
	agent := validAgent("no-runtime", "")
	agent.Spec.Runtime.Type = ""

	ds := mustDS(t, nil,
		map[string]*types.AgentManifest{"no-runtime": agent},
		nil,
	)

	ve := requireValidationError(t, ValidateDesiredState(ds))
	assertErrorMentions(t, ve, "spec.runtime.type")
}

func TestValidateDesiredState_AgentInvalidRuntimeType(t *testing.T) {
	t.Parallel()
	agent := validAgent("bad-runtime", "")
	agent.Spec.Runtime.Type = "docker"

	ds := mustDS(t, nil,
		map[string]*types.AgentManifest{"bad-runtime": agent},
		nil,
	)

	ve := requireValidationError(t, ValidateDesiredState(ds))
	assertErrorMentions(t, ve, "spec.runtime.type")
}

func TestValidateDesiredState_TeamWrongAPIVersion(t *testing.T) {
	t.Parallel()
	team := validTeam("bad-team", "")
	team.APIVersion = "hive/v99"

	ds := mustDS(t, nil, nil,
		map[string]*types.TeamManifest{"bad-team": team},
	)

	ve := requireValidationError(t, ValidateDesiredState(ds))
	assertErrorMentions(t, ve, "apiVersion")
}

func TestValidateDesiredState_TeamWrongKind(t *testing.T) {
	t.Parallel()
	team := validTeam("bad-team", "")
	team.Kind = "Agent"

	ds := mustDS(t, nil, nil,
		map[string]*types.TeamManifest{"bad-team": team},
	)

	ve := requireValidationError(t, ValidateDesiredState(ds))
	assertErrorMentions(t, ve, "kind")
}

// ---------------------------------------------------------------------------
// Tier-specific validation
// ---------------------------------------------------------------------------

func TestValidateDesiredState_InvalidTier(t *testing.T) {
	t.Parallel()
	agent := validAgent("tier-agent", "")
	agent.Spec.Tier = "container"

	ds := mustDS(t, nil,
		map[string]*types.AgentManifest{"tier-agent": agent},
		nil,
	)

	ve := requireValidationError(t, ValidateDesiredState(ds))
	assertErrorMentions(t, ve, "spec.tier")
}

// ---------------------------------------------------------------------------
// Tier validation
// ---------------------------------------------------------------------------

func TestValidateDesiredState_TierRuntimeCompat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		tier    string
		runtime string
		wantErr bool
	}{
		{"vm + openclaw", "vm", "openclaw", false},
		{"vm + custom", "vm", "custom", false},
		{"native + openclaw", "native", "openclaw", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			agent := validAgent("compat-agent", "")
			agent.Spec.Tier = tt.tier
			agent.Spec.Runtime.Type = tt.runtime
			// Custom and process types require a command.
			if tt.runtime == "custom" || tt.runtime == "process" {
				agent.Spec.Runtime.Command = "/usr/bin/test-cmd"
			}

			ds := mustDS(t, nil,
				map[string]*types.AgentManifest{"compat-agent": agent},
				nil,
			)

			err := ValidateDesiredState(ds)
			if tt.wantErr {
				requireError(t, err)
			} else {
				requireNoError(t, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Volumes only valid for vm tier
// ---------------------------------------------------------------------------

func TestValidateDesiredState_VolumesOnlyForVMTier(t *testing.T) {
	t.Parallel()
	agent := validAgent("native-vol", "alpha")
	agent.Spec.Tier = "native"
	agent.Spec.Volumes = []types.AgentVolume{
		{Name: "data", MountPath: "/data"},
	}

	team := validTeam("alpha", "")
	team.Spec.SharedVolumes = []types.SharedVolume{
		{Name: "data", HostPath: "/mnt/data"},
	}

	ds := mustDS(t, nil,
		map[string]*types.AgentManifest{"native-vol": agent},
		map[string]*types.TeamManifest{"alpha": team},
	)

	ve := requireValidationError(t, ValidateDesiredState(ds))
	assertErrorMentions(t, ve, "volumes", "vm tier")
}

// ---------------------------------------------------------------------------
// Agent references nonexistent team
// ---------------------------------------------------------------------------

func TestValidateDesiredState_AgentReferencesNonexistentTeam(t *testing.T) {
	t.Parallel()
	agent := validAgent("lonely-agent", "no-such-team")

	ds := mustDS(t, nil,
		map[string]*types.AgentManifest{"lonely-agent": agent},
		nil,
	)

	ve := requireValidationError(t, ValidateDesiredState(ds))
	assertErrorMentions(t, ve, "metadata.team", "no-such-team", "nonexistent")
}

// ---------------------------------------------------------------------------
// Restart policy validation at agent level
// ---------------------------------------------------------------------------

func TestValidateDesiredState_AgentInvalidRestartPolicy(t *testing.T) {
	t.Parallel()
	agent := validAgent("restart-agent", "")
	agent.Spec.Restart.Policy = "banana"

	ds := mustDS(t, nil,
		map[string]*types.AgentManifest{"restart-agent": agent},
		nil,
	)

	ve := requireValidationError(t, ValidateDesiredState(ds))
	assertErrorMentions(t, ve, "spec.restart.policy")
}

func TestValidateDesiredState_AgentValidRestartPolicies(t *testing.T) {
	t.Parallel()
	policies := []string{"always", "on-failure", "never"}
	for _, p := range policies {
		t.Run(p, func(t *testing.T) {
			t.Parallel()
			agent := validAgent("restart-agent", "")
			agent.Spec.Restart.Policy = p

			ds := mustDS(t, nil,
				map[string]*types.AgentManifest{"restart-agent": agent},
				nil,
			)

			requireNoError(t, ValidateDesiredState(ds))
		})
	}
}

// ---------------------------------------------------------------------------
// Multiple errors collected in a single ValidationError
// ---------------------------------------------------------------------------

func TestValidateDesiredState_MultipleErrors(t *testing.T) {
	t.Parallel()
	// An agent with many problems at once.
	agent := &types.AgentManifest{
		APIVersion: "hive/v2",
		Kind:       "Wrong",
		Metadata:   types.AgentMetadata{ID: "", Team: "no-team"},
		Spec: types.AgentSpec{
			Runtime:   types.AgentRuntime{Type: ""},
			Resources: types.AgentResources{Memory: "bogus", VCPUs: -5},
			Capabilities: []types.AgentCapability{
				{Name: "", Description: ""},
				{Name: "dup", Description: "one"},
				{Name: "dup", Description: "two"},
			},
		},
	}

	ds := mustDS(t, nil,
		map[string]*types.AgentManifest{"": agent},
		nil,
	)

	ve := requireValidationError(t, ValidateDesiredState(ds))

	// Should have collected several errors.
	if len(ve.Errors) < 4 {
		t.Errorf("expected at least 4 validation errors, got %d: %v", len(ve.Errors), ve.Errors)
	}
}

// ---------------------------------------------------------------------------
// Empty DesiredState is valid (no agents, no teams)
// ---------------------------------------------------------------------------

func TestValidateDesiredState_EmptyState(t *testing.T) {
	t.Parallel()
	ds := mustDS(t, nil, nil, nil)
	requireNoError(t, ValidateDesiredState(ds))
}

// ---------------------------------------------------------------------------
// Network egress only for vm tier
// ---------------------------------------------------------------------------

func TestValidateDesiredState_NetworkEgressOnlyForVM(t *testing.T) {
	t.Parallel()
	agent := validAgent("net-agent", "")
	agent.Spec.Tier = "native"
	agent.Spec.Network.Egress = "restricted"

	ds := mustDS(t, nil,
		map[string]*types.AgentManifest{"net-agent": agent},
		nil,
	)

	ve := requireValidationError(t, ValidateDesiredState(ds))
	assertErrorMentions(t, ve, "network egress", "vm tier")
}

// ---------------------------------------------------------------------------
// ValidationError.Error() formatting
// ---------------------------------------------------------------------------

func TestValidationError_ErrorFormat(t *testing.T) {
	t.Parallel()
	ve := &ValidationError{Errors: []string{"one", "two"}}
	s := ve.Error()
	if !strings.Contains(s, "2 error(s)") {
		t.Errorf("Error() = %q, expected mention of 2 errors", s)
	}
	if !strings.Contains(s, "one") || !strings.Contains(s, "two") {
		t.Errorf("Error() = %q, expected both error messages", s)
	}
}

// ---------------------------------------------------------------------------
// Negative duration values
// ---------------------------------------------------------------------------

func TestParseDuration_NegativeValue(t *testing.T) {
	t.Parallel()
	_, err := ParseCluster([]byte(`
apiVersion: hive/v1
kind: Cluster
metadata:
  name: test
spec:
  defaults:
    health:
      interval: "-5s"
    restart:
      policy: on-failure
`))
	requireError(t, err)
	if !strings.Contains(err.Error(), "duration must be non-negative") {
		t.Errorf("expected 'duration must be non-negative' in error, got: %v", err)
	}
}

func TestParseDuration_ZeroValue(t *testing.T) {
	t.Parallel()
	_, err := ParseCluster([]byte(`
apiVersion: hive/v1
kind: Cluster
metadata:
  name: test
spec:
  defaults:
    health:
      interval: "0s"
    restart:
      policy: on-failure
`))
	if err != nil {
		t.Errorf("expected zero duration to be accepted, got error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// NATS mode validation
// ---------------------------------------------------------------------------

func TestValidateCluster_NATSModeValid(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"embedded", "external"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			_, err := ParseCluster([]byte(`
apiVersion: hive/v1
kind: Cluster
metadata:
  name: test
spec:
  nats:
    mode: ` + mode + `
  defaults:
    restart:
      policy: on-failure
`))
			requireNoError(t, err)
		})
	}
}

func TestValidateCluster_NATSModeInvalid(t *testing.T) {
	t.Parallel()
	_, err := ParseCluster([]byte(`
apiVersion: hive/v1
kind: Cluster
metadata:
  name: test
spec:
  nats:
    mode: standalone
  defaults:
    restart:
      policy: on-failure
`))
	requireError(t, err)
	if !strings.Contains(err.Error(), "spec.nats.mode") {
		t.Errorf("expected 'spec.nats.mode' in error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Dashboard/Metrics address format
// ---------------------------------------------------------------------------

func TestValidateCluster_DashboardAddrInvalid(t *testing.T) {
	t.Parallel()
	_, err := ParseCluster([]byte(`
apiVersion: hive/v1
kind: Cluster
metadata:
  name: test
spec:
  dashboard:
    enabled: true
    addr: "no-port-here"
  defaults:
    restart:
      policy: on-failure
`))
	requireError(t, err)
	if !strings.Contains(err.Error(), "spec.dashboard.addr") {
		t.Errorf("expected 'spec.dashboard.addr' in error, got: %v", err)
	}
}

func TestValidateCluster_DashboardAddrValid(t *testing.T) {
	t.Parallel()
	_, err := ParseCluster([]byte(`
apiVersion: hive/v1
kind: Cluster
metadata:
  name: test
spec:
  dashboard:
    enabled: true
    addr: ":8080"
  defaults:
    restart:
      policy: on-failure
`))
	requireNoError(t, err)
}

func TestValidateCluster_MetricsAddrInvalid(t *testing.T) {
	t.Parallel()
	_, err := ParseCluster([]byte(`
apiVersion: hive/v1
kind: Cluster
metadata:
  name: test
spec:
  metrics:
    enabled: true
    addr: "bad-addr"
  defaults:
    restart:
      policy: on-failure
`))
	requireError(t, err)
	if !strings.Contains(err.Error(), "spec.metrics.addr") {
		t.Errorf("expected 'spec.metrics.addr' in error, got: %v", err)
	}
}

func TestValidateCluster_MetricsAddrValid(t *testing.T) {
	t.Parallel()
	_, err := ParseCluster([]byte(`
apiVersion: hive/v1
kind: Cluster
metadata:
  name: test
spec:
  metrics:
    enabled: true
    addr: "0.0.0.0:9090"
  defaults:
    restart:
      policy: on-failure
`))
	requireNoError(t, err)
}

// ---------------------------------------------------------------------------
// Logging retention days
// ---------------------------------------------------------------------------

func TestValidateCluster_LoggingRetentionInvalid(t *testing.T) {
	t.Parallel()
	_, err := ParseCluster([]byte(`
apiVersion: hive/v1
kind: Cluster
metadata:
  name: test
spec:
  logging:
    enabled: true
    retentionDays: -5
  defaults:
    restart:
      policy: on-failure
`))
	requireError(t, err)
	if !strings.Contains(err.Error(), "retentionDays") {
		t.Errorf("expected 'retentionDays' in error, got: %v", err)
	}
}

func TestValidateCluster_LoggingRetentionValid(t *testing.T) {
	t.Parallel()
	_, err := ParseCluster([]byte(`
apiVersion: hive/v1
kind: Cluster
metadata:
  name: test
spec:
  logging:
    enabled: true
    retentionDays: 30
  defaults:
    restart:
      policy: on-failure
`))
	requireNoError(t, err)
}

// ---------------------------------------------------------------------------
// MaxRestarts and MaxFailures negative values
// ---------------------------------------------------------------------------

func TestValidateCluster_NegativeMaxRestarts(t *testing.T) {
	t.Parallel()
	_, err := ParseCluster([]byte(`
apiVersion: hive/v1
kind: Cluster
metadata:
  name: test
spec:
  defaults:
    restart:
      policy: on-failure
      maxRestarts: -1
`))
	requireError(t, err)
	if !strings.Contains(err.Error(), "maxRestarts") {
		t.Errorf("expected 'maxRestarts' in error, got: %v", err)
	}
}

func TestValidateCluster_NegativeMaxFailures(t *testing.T) {
	t.Parallel()
	_, err := ParseCluster([]byte(`
apiVersion: hive/v1
kind: Cluster
metadata:
  name: test
spec:
  defaults:
    health:
      maxFailures: -3
    restart:
      policy: on-failure
`))
	requireError(t, err)
	if !strings.Contains(err.Error(), "maxFailures") {
		t.Errorf("expected 'maxFailures' in error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ParseMemory rejects zero
// ---------------------------------------------------------------------------

func TestParseMemory_RejectsZero(t *testing.T) {
	t.Parallel()
	_, err := ParseMemory("0Mi")
	requireError(t, err)
	if !strings.Contains(err.Error(), "must be positive") {
		t.Errorf("expected 'must be positive' in error, got: %v", err)
	}
}

func TestParseMemory_RejectsZeroBytes(t *testing.T) {
	t.Parallel()
	_, err := ParseMemory("0")
	requireError(t, err)
	if !strings.Contains(err.Error(), "must be positive") {
		t.Errorf("expected 'must be positive' in error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Dangerous mount guest paths
// ---------------------------------------------------------------------------

func TestValidateDesiredState_MountDangerousGuestPath(t *testing.T) {
	t.Parallel()
	dangerousPaths := []string{"/", "/etc", "/usr", "/bin", "/sbin", "/lib", "/dev", "/proc", "/sys"}

	for _, path := range dangerousPaths {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			agent := validAgent("mount-agent", "")
			agent.Spec.Mounts = []types.AgentMount{
				{Name: "danger", Host: "/opt/host", Guest: path, Mode: "ro"},
			}

			ds := mustDS(t, nil,
				map[string]*types.AgentManifest{"mount-agent": agent},
				nil,
			)

			ve := requireValidationError(t, ValidateDesiredState(ds))
			assertErrorMentions(t, ve, "dangerous system path")
		})
	}
}

func TestValidateDesiredState_MountGuestPathTraversal(t *testing.T) {
	t.Parallel()
	agent := validAgent("mount-agent", "")
	agent.Spec.Mounts = []types.AgentMount{
		{Name: "traversal", Host: "/opt/host", Guest: "/data/../etc/passwd", Mode: "ro"},
	}

	ds := mustDS(t, nil,
		map[string]*types.AgentManifest{"mount-agent": agent},
		nil,
	)

	ve := requireValidationError(t, ValidateDesiredState(ds))
	assertErrorMentions(t, ve, "path traversal")
}

func TestValidateDesiredState_MountValidGuestPath(t *testing.T) {
	t.Parallel()
	agent := validAgent("mount-agent", "")
	agent.Spec.Mounts = []types.AgentMount{
		{Name: "valid", Host: "/opt/host", Guest: "/data/workdir", Mode: "ro"},
	}

	ds := mustDS(t, nil,
		map[string]*types.AgentManifest{"mount-agent": agent},
		nil,
	)

	requireNoError(t, ValidateDesiredState(ds))
}

// ---------------------------------------------------------------------------
// Ingress path validation
// ---------------------------------------------------------------------------

func TestValidateDesiredState_IngressPathTraversal(t *testing.T) {
	t.Parallel()
	agent := validAgent("ingress-agent", "")
	agent.Spec.Ingress = types.AgentIngress{Port: 8080, Path: "/api/../admin"}

	ds := mustDS(t, nil,
		map[string]*types.AgentManifest{"ingress-agent": agent},
		nil,
	)

	ve := requireValidationError(t, ValidateDesiredState(ds))
	assertErrorMentions(t, ve, "path traversal")
}

func TestValidateDesiredState_IngressPathInvalidChars(t *testing.T) {
	t.Parallel()
	agent := validAgent("ingress-agent", "")
	agent.Spec.Ingress = types.AgentIngress{Port: 8080, Path: "/api/<script>"}

	ds := mustDS(t, nil,
		map[string]*types.AgentManifest{"ingress-agent": agent},
		nil,
	)

	ve := requireValidationError(t, ValidateDesiredState(ds))
	assertErrorMentions(t, ve, "invalid characters")
}

func TestValidateDesiredState_IngressPathValid(t *testing.T) {
	t.Parallel()
	agent := validAgent("ingress-agent", "")
	agent.Spec.Ingress = types.AgentIngress{Port: 8080, Path: "/api/v1/agents"}

	ds := mustDS(t, nil,
		map[string]*types.AgentManifest{"ingress-agent": agent},
		nil,
	)

	requireNoError(t, ValidateDesiredState(ds))
}

// ---------------------------------------------------------------------------
// SecretsFile validation in ValidateDesiredState
// ---------------------------------------------------------------------------

func TestValidateDesiredState_SecretsFileNotFound(t *testing.T) {
	t.Parallel()
	cluster := validCluster()
	cluster.Spec.SecretsFile = "/nonexistent/path/to/secrets.env"

	ds := mustDS(t, cluster, nil, nil)
	ve := requireValidationError(t, ValidateDesiredState(ds))
	assertErrorMentions(t, ve, "spec.secretsFile", "/nonexistent/path/to/secrets.env")
}

func TestValidateDesiredState_SecretsFileExists(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	secretsPath := filepath.Join(dir, "secrets.env")
	if err := os.WriteFile(secretsPath, []byte("API_KEY=test"), 0600); err != nil {
		t.Fatal(err)
	}

	cluster := validCluster()
	cluster.Spec.SecretsFile = secretsPath

	ds := mustDS(t, cluster, nil, nil)
	requireNoError(t, ValidateDesiredState(ds))
}

func TestValidateDesiredState_SecretsFileEmpty(t *testing.T) {
	t.Parallel()
	cluster := validCluster()
	cluster.Spec.SecretsFile = ""

	ds := mustDS(t, cluster, nil, nil)
	requireNoError(t, ValidateDesiredState(ds))
}

func TestValidateDesiredState_ModeValid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		tier string
		mode string
	}{
		{"managed native", "native", "managed"},
		{"external native", "native", "external"},
		{"managed vm", "vm", "managed"},
		{"empty mode native", "native", ""},
		{"empty mode vm", "vm", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			agent := validAgent("test-agent", "")
			agent.Spec.Tier = tt.tier
			agent.Spec.Mode = tt.mode
			ds := mustDS(t, nil, map[string]*types.AgentManifest{"test-agent": agent}, nil)
			requireNoError(t, ValidateDesiredState(ds))
		})
	}
}

func TestValidateDesiredState_ModeInvalid(t *testing.T) {
	t.Parallel()
	agent := validAgent("test-agent", "")
	agent.Spec.Mode = "bogus"
	ds := mustDS(t, nil, map[string]*types.AgentManifest{"test-agent": agent}, nil)
	ve := requireValidationError(t, ValidateDesiredState(ds))
	assertErrorMentions(t, ve, "spec.mode must be one of [managed, external]")
}

func TestValidateDesiredState_ModeVMExternalInvalid(t *testing.T) {
	t.Parallel()
	agent := validAgent("test-agent", "")
	agent.Spec.Tier = "vm"
	agent.Spec.Mode = "external"
	ds := mustDS(t, nil, map[string]*types.AgentManifest{"test-agent": agent}, nil)
	ve := requireValidationError(t, ValidateDesiredState(ds))
	assertErrorMentions(t, ve, `spec.mode "external" is invalid for tier "vm"`)
}
