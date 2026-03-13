// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// TestFirecrackerBackendSelection verifies that hived correctly selects the
// process backend when --force-process-backend is set, and that VM-tier agents
// run successfully on the process backend.
func TestFirecrackerBackendSelection(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}

	binDir := buildBinaries(t)
	port := freePort(t)
	clusterRoot := createFirecrackerCluster(t, port)

	// Write a VM-tier agent manifest.
	writeVMAgentManifest(t, clusterRoot, "vm-agent", "default", "512Mi", 1, "")

	// Start hived with --force-process-backend — VM-tier agents should use process backend.
	startHivedWithFlags(t, binDir, clusterRoot, port, "--force-process-backend")

	natsAuthTokenPath := filepath.Join(clusterRoot, ".state", "nats-auth-token")
	waitForFile(t, natsAuthTokenPath, 15*time.Second)
	natsAuthToken := readFile(t, natsAuthTokenPath)
	natsURL := fmt.Sprintf("nats://127.0.0.1:%d", port)

	nc, err := nats.Connect(natsURL, nats.Token(natsAuthToken), nats.Timeout(10*time.Second))
	if err != nil {
		t.Fatalf("connecting to NATS: %v", err)
	}
	defer nc.Close()

	t.Run("vm_tier_agent_starts_on_process_backend", func(t *testing.T) {
		out := runHivectlWithPort(t, binDir, clusterRoot, port, "agents", "start", "vm-agent")
		if !strings.Contains(out, "started") {
			t.Fatalf("expected 'started' in output, got: %s", out)
		}

		waitForAgentRunning(t, binDir, clusterRoot, port, "vm-agent", 10*time.Second)
	})

	t.Run("vm_tier_agent_stops_cleanly", func(t *testing.T) {
		out := runHivectlWithPort(t, binDir, clusterRoot, port, "agents", "stop", "vm-agent")
		if !strings.Contains(out, "stopped") {
			t.Fatalf("expected 'stopped' in output, got: %s", out)
		}
	})

	t.Run("vm_tier_agent_destroys_cleanly", func(t *testing.T) {
		out := runHivectlWithPort(t, binDir, clusterRoot, port, "agents", "destroy", "vm-agent")
		if !strings.Contains(out, "destroyed") {
			t.Fatalf("expected 'destroyed' in output, got: %s", out)
		}
	})
}

// TestFirecrackerNetworkPolicyConfig verifies that agents with different network
// policy configurations (egress: none, restricted, full) are accepted by the
// config validator and start successfully on the process backend.
//
// NOTE: The process backend does not enforce network policy (nftables rules are
// only applied inside Firecracker VMs). This test validates that manifests with
// network settings are parsed, accepted, and that agents reach RUNNING state.
// TODO: Add VM-level network enforcement tests that require KVM (/dev/kvm).
func TestFirecrackerNetworkPolicyConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}

	binDir := buildBinaries(t)
	port := freePort(t)
	clusterRoot := createFirecrackerCluster(t, port)

	// Write all agent manifests before starting hived.
	writeVMAgentManifestWithNetwork(t, clusterRoot, "no-egress-agent", "default", "none", nil)
	writeVMAgentManifestWithNetwork(t, clusterRoot, "restricted-agent", "default", "restricted",
		[]string{"api.anthropic.com", "github.com"})
	writeVMAgentManifestWithNetwork(t, clusterRoot, "full-egress-agent", "default", "full", nil)

	// Start hived at the parent level so it stays alive across all subtests.
	startHivedWithFlags(t, binDir, clusterRoot, port, "--force-process-backend")

	natsAuthTokenPath := filepath.Join(clusterRoot, ".state", "nats-auth-token")
	waitForFile(t, natsAuthTokenPath, 15*time.Second)

	t.Run("egress_none_agent_starts_and_is_running", func(t *testing.T) {
		out := runHivectlWithPort(t, binDir, clusterRoot, port, "agents", "start", "no-egress-agent")
		if !strings.Contains(out, "started") {
			t.Fatalf("expected 'started' in output, got: %s", out)
		}

		waitForAgentRunning(t, binDir, clusterRoot, port, "no-egress-agent", 10*time.Second)

		// Verify the agent is listed as RUNNING via hivectl agents list.
		listOut := runHivectlWithPort(t, binDir, clusterRoot, port, "agents", "list")
		if !strings.Contains(listOut, "no-egress-agent") {
			t.Fatalf("expected no-egress-agent in agents list, got: %s", listOut)
		}
	})

	t.Run("egress_restricted_agent_starts_and_is_running", func(t *testing.T) {
		out := runHivectlWithPort(t, binDir, clusterRoot, port, "agents", "start", "restricted-agent")
		if !strings.Contains(out, "started") {
			t.Fatalf("expected 'started' in output, got: %s", out)
		}

		waitForAgentRunning(t, binDir, clusterRoot, port, "restricted-agent", 10*time.Second)

		// Verify the agent is listed as RUNNING via hivectl agents list.
		listOut := runHivectlWithPort(t, binDir, clusterRoot, port, "agents", "list")
		if !strings.Contains(listOut, "restricted-agent") {
			t.Fatalf("expected restricted-agent in agents list, got: %s", listOut)
		}
		// Verify agent status JSON shows RUNNING state.
		statusOut := runHivectlWithPort(t, binDir, clusterRoot, port, "agents", "status", "restricted-agent")
		assertAgentStatusField(t, statusOut, "restricted-agent", "status", "RUNNING")
	})

	t.Run("egress_full_agent_starts_and_is_running", func(t *testing.T) {
		out := runHivectlWithPort(t, binDir, clusterRoot, port, "agents", "start", "full-egress-agent")
		if !strings.Contains(out, "started") {
			t.Fatalf("expected 'started' in output, got: %s", out)
		}

		waitForAgentRunning(t, binDir, clusterRoot, port, "full-egress-agent", 10*time.Second)

		// Verify the agent is listed as RUNNING via hivectl agents list.
		listOut := runHivectlWithPort(t, binDir, clusterRoot, port, "agents", "list")
		if !strings.Contains(listOut, "full-egress-agent") {
			t.Fatalf("expected full-egress-agent in agents list, got: %s", listOut)
		}
	})
}

// TestFirecrackerResourceLimits verifies that resource limits from manifests
// are properly parsed and stored when creating agents on the process backend.
//
// NOTE: The process backend does not enforce resource limits (memory/CPU cgroups
// are only applied inside Firecracker VMs). This test validates that manifests
// with resource settings are parsed, that agents reach RUNNING state, and that
// the stored agent state reflects the configured resource values.
// TODO: Add VM-level resource enforcement tests that require KVM (/dev/kvm).
func TestFirecrackerResourceLimits(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}

	binDir := buildBinaries(t)
	port := freePort(t)
	clusterRoot := createFirecrackerCluster(t, port)

	// Agent with explicit resource limits.
	writeVMAgentManifest(t, clusterRoot, "resource-agent", "default", "256Mi", 2, "2Gi")

	startHivedWithFlags(t, binDir, clusterRoot, port, "--force-process-backend")

	natsAuthTokenPath := filepath.Join(clusterRoot, ".state", "nats-auth-token")
	waitForFile(t, natsAuthTokenPath, 15*time.Second)

	t.Run("agent_with_resource_limits_starts_and_stores_config", func(t *testing.T) {
		out := runHivectlWithPort(t, binDir, clusterRoot, port, "agents", "start", "resource-agent")
		if !strings.Contains(out, "started") {
			t.Fatalf("expected 'started' in output, got: %s", out)
		}

		waitForAgentRunning(t, binDir, clusterRoot, port, "resource-agent", 10*time.Second)

		// Verify the agent status JSON shows the configured resource values.
		statusOut := runHivectlWithPort(t, binDir, clusterRoot, port, "agents", "status", "resource-agent")
		assertAgentStatusField(t, statusOut, "resource-agent", "status", "RUNNING")
		// 256Mi = 268435456 bytes.
		assertAgentStatusField(t, statusOut, "resource-agent", "memory_bytes", float64(268435456))
		assertAgentStatusField(t, statusOut, "resource-agent", "vcpus", float64(2))
	})

	t.Run("agent_with_default_resources_starts_and_stores_config", func(t *testing.T) {
		// Agent with no explicit resources (should use defaults: 512Mi, 1 vCPU).
		writeVMAgentManifest(t, clusterRoot, "default-resource-agent", "default", "", 0, "")

		out := runHivectlWithPort(t, binDir, clusterRoot, port, "agents", "start", "default-resource-agent")
		if !strings.Contains(out, "started") {
			t.Fatalf("expected 'started' in output, got: %s", out)
		}

		waitForAgentRunning(t, binDir, clusterRoot, port, "default-resource-agent", 10*time.Second)

		// Verify the agent status JSON shows RUNNING and default resource values.
		statusOut := runHivectlWithPort(t, binDir, clusterRoot, port, "agents", "status", "default-resource-agent")
		assertAgentStatusField(t, statusOut, "default-resource-agent", "status", "RUNNING")
		// 512Mi = 536870912 bytes (cluster default).
		assertAgentStatusField(t, statusOut, "default-resource-agent", "memory_bytes", float64(536870912))
		assertAgentStatusField(t, statusOut, "default-resource-agent", "vcpus", float64(1))
	})
}

// TestFirecrackerSharedVolumes verifies that shared volumes are properly
// validated and that agents with volume configurations start successfully
// on the process backend.
//
// NOTE: The process backend does not mount shared volumes (virtio-fs mounts are
// only configured inside Firecracker VMs). This test validates that manifests
// with volume settings are parsed, accepted, and that agents reach RUNNING state.
// TODO: Add VM-level shared volume tests that require KVM (/dev/kvm).
func TestFirecrackerSharedVolumes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}

	binDir := buildBinaries(t)
	port := freePort(t)
	clusterRoot := createFirecrackerClusterWithSharedVolumes(t, port)

	startHivedWithFlags(t, binDir, clusterRoot, port, "--force-process-backend")

	natsAuthTokenPath := filepath.Join(clusterRoot, ".state", "nats-auth-token")
	waitForFile(t, natsAuthTokenPath, 15*time.Second)

	t.Run("agent_with_shared_volume_starts_and_is_running", func(t *testing.T) {
		out := runHivectlWithPort(t, binDir, clusterRoot, port, "agents", "start", "writer-agent")
		if !strings.Contains(out, "started") {
			t.Fatalf("expected 'started' in output, got: %s", out)
		}

		waitForAgentRunning(t, binDir, clusterRoot, port, "writer-agent", 10*time.Second)

		// Verify the agent shows as RUNNING in hivectl agents list.
		listOut := runHivectlWithPort(t, binDir, clusterRoot, port, "agents", "list")
		if !strings.Contains(listOut, "writer-agent") {
			t.Fatalf("expected writer-agent in agents list, got: %s", listOut)
		}
		// Verify agent status JSON confirms RUNNING state.
		statusOut := runHivectlWithPort(t, binDir, clusterRoot, port, "agents", "status", "writer-agent")
		assertAgentStatusField(t, statusOut, "writer-agent", "status", "RUNNING")
	})

	t.Run("second_agent_with_shared_volume_starts_and_is_running", func(t *testing.T) {
		out := runHivectlWithPort(t, binDir, clusterRoot, port, "agents", "start", "reader-agent")
		if !strings.Contains(out, "started") {
			t.Fatalf("expected 'started' in output, got: %s", out)
		}

		waitForAgentRunning(t, binDir, clusterRoot, port, "reader-agent", 10*time.Second)

		// Verify the agent shows as RUNNING in hivectl agents list.
		listOut := runHivectlWithPort(t, binDir, clusterRoot, port, "agents", "list")
		if !strings.Contains(listOut, "reader-agent") {
			t.Fatalf("expected reader-agent in agents list, got: %s", listOut)
		}
		// Verify agent status JSON confirms RUNNING state.
		statusOut := runHivectlWithPort(t, binDir, clusterRoot, port, "agents", "status", "reader-agent")
		assertAgentStatusField(t, statusOut, "reader-agent", "status", "RUNNING")
	})
}

// TestFirecrackerCapabilityLifecycle verifies full capability registration and
// invocation through the Firecracker backend (via process fallback).
func TestFirecrackerCapabilityLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}

	binDir := buildBinaries(t)
	port := freePort(t)
	clusterRoot := createFirecrackerCluster(t, port)

	// Write a native agent manifest with capabilities (joins via hive-agent).
	startHivedWithFlags(t, binDir, clusterRoot, port, "--force-process-backend")

	natsAuthTokenPath := filepath.Join(clusterRoot, ".state", "nats-auth-token")
	waitForFile(t, natsAuthTokenPath, 15*time.Second)
	natsAuthToken := readFile(t, natsAuthTokenPath)
	natsURL := fmt.Sprintf("nats://127.0.0.1:%d", port)

	nc, err := nats.Connect(natsURL, nats.Token(natsAuthToken), nats.Timeout(10*time.Second))
	if err != nil {
		t.Fatalf("connecting to NATS: %v", err)
	}
	defer nc.Close()

	// Pre-seed a join token.
	rawToken := "bbccddeeff00112233445566778899aabbccddeeff00112233445566778899ab"
	preSeedToken(t, clusterRoot, rawToken)

	// Start a native agent with capabilities.
	nativeManifestPath := writeNativeAgentManifest(t)
	agentCmd := exec.Command(
		filepath.Join(binDir, "hive-agent"), "join",
		"--token", rawToken,
		"--control-plane", fmt.Sprintf("127.0.0.1:%d", port),
		"--agent-id", "fc-native-agent",
		"--nats-token", natsAuthToken,
		"--http-addr", ":0",
		"--work-dir", t.TempDir(),
		"--manifest", nativeManifestPath,
	)
	agentCmd.Env = append(os.Environ(), "HIVE_TEST_FIRECRACKER=mock")
	agentCmd.Stdout = os.Stderr
	agentCmd.Stderr = os.Stderr

	if err := agentCmd.Start(); err != nil {
		t.Fatalf("starting hive-agent: %v", err)
	}
	t.Cleanup(func() {
		agentCmd.Process.Signal(os.Interrupt)
		agentCmd.Wait() //nolint:errcheck
	})

	t.Run("capability_invocation", func(t *testing.T) {
		// Wait for agent to register capabilities via heartbeat.
		sub, err := nc.SubscribeSync("hive.health.fc-native-agent")
		if err != nil {
			t.Fatalf("subscribing: %v", err)
		}
		defer sub.Unsubscribe()
		nc.Flush()

		_, err = sub.NextMsg(35 * time.Second)
		if err != nil {
			t.Fatalf("waiting for heartbeat: %v", err)
		}

		// Invoke capability.
		capSubject := "hive.capabilities.fc-native-agent.summarize.request"
		envelope := map[string]interface{}{
			"id":        "fc-test-001",
			"from":      "fc-e2e-test",
			"to":        "fc-native-agent",
			"type":      "capability-request",
			"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
			"payload": map[string]interface{}{
				"capability": "summarize",
				"inputs":     map[string]interface{}{"text": "test input"},
				"timeout":    "10s",
			},
		}
		reqData, err := json.Marshal(envelope)
		if err != nil {
			t.Fatalf("marshaling request: %v", err)
		}

		msg, err := nc.Request(capSubject, reqData, 10*time.Second)
		if err != nil {
			t.Fatalf("capability request failed: %v", err)
		}

		var respEnv map[string]interface{}
		if err := json.Unmarshal(msg.Data, &respEnv); err != nil {
			t.Fatalf("parsing response: %v", err)
		}

		payload, ok := respEnv["payload"].(map[string]interface{})
		if !ok {
			t.Fatalf("expected payload in response, got: %v", respEnv)
		}

		status, _ := payload["status"].(string)
		if status != "success" {
			t.Fatalf("expected status=success, got: %v", payload)
		}
		t.Logf("capability invocation succeeded: %v", payload)
	})
}

// TestFirecrackerAutoFallback verifies that hived automatically falls back to
// the process backend when KVM is not available (which is the case on macOS
// and CI without KVM).
func TestFirecrackerAutoFallback(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}

	// This test verifies the auto-fallback behavior — on macOS or without
	// KVM, hived should fall back to process backend without --force-process-backend.
	if runtime.GOOS == "linux" {
		if _, err := os.Stat("/dev/kvm"); err == nil {
			t.Skip("skipping auto-fallback test: KVM is available (test is for non-KVM environments)")
		}
	}

	binDir := buildBinaries(t)
	port := freePort(t)
	clusterRoot := createFirecrackerCluster(t, port)
	writeVMAgentManifest(t, clusterRoot, "fallback-agent", "default", "256Mi", 1, "")

	// Start hived WITHOUT --force-process-backend — should auto-detect no KVM and fall back.
	startHivedWithFlags(t, binDir, clusterRoot, port)

	natsAuthTokenPath := filepath.Join(clusterRoot, ".state", "nats-auth-token")
	waitForFile(t, natsAuthTokenPath, 15*time.Second)

	t.Run("vm_agent_runs_via_auto_fallback", func(t *testing.T) {
		out := runHivectlWithPort(t, binDir, clusterRoot, port, "agents", "start", "fallback-agent")
		if !strings.Contains(out, "started") {
			t.Fatalf("expected 'started' in output, got: %s", out)
		}

		waitForAgentRunning(t, binDir, clusterRoot, port, "fallback-agent", 10*time.Second)
	})
}

// TestFirecrackerManifestValidation verifies that invalid manifests are
// properly rejected by the config validation layer.
func TestFirecrackerManifestValidation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}

	binDir := buildBinaries(t)

	t.Run("invalid_egress_mode_rejected", func(t *testing.T) {
		port := freePort(t)
		clusterRoot := createFirecrackerCluster(t, port)
		writeVMAgentManifestWithNetwork(t, clusterRoot, "bad-egress-agent", "default", "invalid-mode", nil)

		out, err := runHivectlValidateWithError(t, binDir, clusterRoot)
		if err == nil {
			t.Fatalf("expected validation to fail for invalid egress mode, but it succeeded; output: %s", out)
		}
		if !strings.Contains(strings.ToLower(out), "egress") {
			t.Errorf("expected validation output to mention 'egress', got: %s", out)
		}
	})

	t.Run("allowlist_without_restricted_rejected", func(t *testing.T) {
		port := freePort(t)
		clusterRoot := createFirecrackerCluster(t, port)
		writeVMAgentManifestWithNetwork(t, clusterRoot, "bad-allowlist-agent", "default", "full",
			[]string{"example.com"})

		out, err := runHivectlValidateWithError(t, binDir, clusterRoot)
		if err == nil {
			t.Fatalf("expected validation to fail for allowlist without restricted egress, but it succeeded; output: %s", out)
		}
		if !strings.Contains(strings.ToLower(out), "allowlist") {
			t.Errorf("expected validation output to mention 'allowlist', got: %s", out)
		}
	})
}

// --- Helpers ---

func createFirecrackerCluster(t *testing.T, port int) string {
	t.Helper()

	root := fmt.Sprintf("/tmp/hive-fc-e2e-%d", rand.Int63())
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatalf("creating cluster root: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })

	clusterYAML := fmt.Sprintf(`apiVersion: hive/v1
kind: Cluster
metadata:
  name: fc-e2e-cluster
spec:
  nats:
    port: %d
    clusterPort: -1
    jetstream:
      enabled: true
  defaults:
    resources:
      memory: "512Mi"
      vcpus: 1
    health:
      interval: "5s"
      timeout: "3s"
      maxFailures: 3
    restart:
      policy: on-failure
      maxRestarts: 3
      backoff: "2s"
`, port)

	if err := os.WriteFile(filepath.Join(root, "cluster.yaml"), []byte(clusterYAML), 0644); err != nil {
		t.Fatalf("writing cluster.yaml: %v", err)
	}

	// Dummy rootfs files for mock hypervisor.
	rootfsDir := filepath.Join(root, "rootfs")
	if err := os.MkdirAll(rootfsDir, 0755); err != nil {
		t.Fatalf("creating rootfs dir: %v", err)
	}
	os.WriteFile(filepath.Join(rootfsDir, "vmlinux"), []byte("fake-kernel"), 0644)     //nolint:errcheck
	os.WriteFile(filepath.Join(rootfsDir, "rootfs.ext4"), []byte("fake-rootfs"), 0644) //nolint:errcheck

	// Default team.
	teamsDir := filepath.Join(root, "teams")
	os.MkdirAll(teamsDir, 0755) //nolint:errcheck
	teamManifest := `apiVersion: hive/v1
kind: Team
metadata:
  id: default
spec:
  lead: vm-agent
`
	os.WriteFile(filepath.Join(teamsDir, "default.yaml"), []byte(teamManifest), 0644) //nolint:errcheck

	return root
}

func createFirecrackerClusterWithSharedVolumes(t *testing.T, port int) string {
	t.Helper()

	root := createFirecrackerCluster(t, port)

	// Create a shared volume directory.
	sharedDir := filepath.Join(root, "shared-data")
	if err := os.MkdirAll(sharedDir, 0755); err != nil {
		t.Fatalf("creating shared dir: %v", err)
	}
	// Write a test file in the shared directory.
	os.WriteFile(filepath.Join(sharedDir, "test.txt"), []byte("shared content"), 0644) //nolint:errcheck

	// Update team manifest with shared volumes.
	teamsDir := filepath.Join(root, "teams")
	teamManifest := fmt.Sprintf(`apiVersion: hive/v1
kind: Team
metadata:
  id: default
spec:
  lead: writer-agent
  shared_volumes:
    - name: data
      hostPath: %s
`, sharedDir)
	os.WriteFile(filepath.Join(teamsDir, "default.yaml"), []byte(teamManifest), 0644) //nolint:errcheck

	// Writer agent (rw access).
	writeVMAgentManifestWithVolumes(t, root, "writer-agent", "default", "data", "/workspace/data", "rw")
	// Reader agent (ro access).
	writeVMAgentManifestWithVolumes(t, root, "reader-agent", "default", "data", "/workspace/data", "ro")

	return root
}

func writeVMAgentManifest(t *testing.T, clusterRoot, agentID, team, memory string, vcpus int, disk string) {
	t.Helper()

	agentDir := filepath.Join(clusterRoot, "agents", agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("creating agent dir: %v", err)
	}

	resources := "  resources:\n"
	if memory != "" {
		resources += fmt.Sprintf("    memory: %q\n", memory)
	}
	if vcpus > 0 {
		resources += fmt.Sprintf("    vcpus: %d\n", vcpus)
	}
	if disk != "" {
		resources += fmt.Sprintf("    disk: %q\n", disk)
	}

	manifest := fmt.Sprintf(`apiVersion: hive/v1
kind: Agent
metadata:
  id: %s
  team: %s
spec:
  tier: vm
  runtime:
    type: noop
%s  capabilities:
    - name: test-cap
      description: Test capability
      inputs:
        - name: input
          type: string
      outputs:
        - name: output
          type: string
`, agentID, team, resources)

	if err := os.WriteFile(filepath.Join(agentDir, "manifest.yaml"), []byte(manifest), 0644); err != nil {
		t.Fatalf("writing agent manifest: %v", err)
	}
}

func writeVMAgentManifestWithNetwork(t *testing.T, clusterRoot, agentID, team, egress string, allowlist []string) {
	t.Helper()

	agentDir := filepath.Join(clusterRoot, "agents", agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("creating agent dir: %v", err)
	}

	network := "  network:\n"
	network += fmt.Sprintf("    egress: %s\n", egress)
	if len(allowlist) > 0 {
		network += "    egress_allowlist:\n"
		for _, domain := range allowlist {
			network += fmt.Sprintf("      - %s\n", domain)
		}
	}

	manifest := fmt.Sprintf(`apiVersion: hive/v1
kind: Agent
metadata:
  id: %s
  team: %s
spec:
  tier: vm
  runtime:
    type: noop
  resources:
    memory: "256Mi"
    vcpus: 1
%s  capabilities:
    - name: test-cap
      description: Test capability
`, agentID, team, network)

	if err := os.WriteFile(filepath.Join(agentDir, "manifest.yaml"), []byte(manifest), 0644); err != nil {
		t.Fatalf("writing agent manifest: %v", err)
	}
}

func writeVMAgentManifestWithVolumes(t *testing.T, clusterRoot, agentID, team, volName, mountPath, access string) {
	t.Helper()

	agentDir := filepath.Join(clusterRoot, "agents", agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("creating agent dir: %v", err)
	}

	manifest := fmt.Sprintf(`apiVersion: hive/v1
kind: Agent
metadata:
  id: %s
  team: %s
spec:
  tier: vm
  runtime:
    type: noop
  resources:
    memory: "256Mi"
    vcpus: 1
  volumes:
    - name: %s
      mountPath: %s
      access: %s
  capabilities:
    - name: test-cap
      description: Test capability
`, agentID, team, volName, mountPath, access)

	if err := os.WriteFile(filepath.Join(agentDir, "manifest.yaml"), []byte(manifest), 0644); err != nil {
		t.Fatalf("writing agent manifest: %v", err)
	}
}

func waitForAgentRunning(t *testing.T, binDir, clusterRoot string, port int, agentID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var status string
	for time.Now().Before(deadline) {
		status = runHivectlWithPort(t, binDir, clusterRoot, port, "agents", "status", agentID)
		if strings.Contains(status, "RUNNING") {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("%s did not reach RUNNING state within %s, last status: %s", agentID, timeout, status)
}

func startHivedWithFlags(t *testing.T, binDir, clusterRoot string, port int, extraFlags ...string) {
	t.Helper()

	args := []string{"--cluster-root", clusterRoot}
	args = append(args, extraFlags...)

	cmd := exec.Command(filepath.Join(binDir, "hived"), args...)
	cmd.Env = append(os.Environ(),
		"HIVE_TEST_FIRECRACKER=mock",
		fmt.Sprintf("HIVE_NATS_PORT=%d", port),
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("starting hived: %v", err)
	}
	t.Logf("hived started (pid %d) with flags %v", cmd.Process.Pid, extraFlags)

	t.Cleanup(func() {
		cmd.Process.Signal(os.Interrupt) //nolint:errcheck
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			cmd.Process.Kill() //nolint:errcheck
			<-done
		}
	})
}

func runHivectlWithPort(t *testing.T, binDir, clusterRoot string, port int, args ...string) string {
	t.Helper()

	fullArgs := append([]string{"--cluster-root", clusterRoot}, args...)
	cmd := exec.Command(filepath.Join(binDir, "hivectl"), fullArgs...)
	cmd.Env = append(os.Environ(),
		"HIVE_TEST_FIRECRACKER=mock",
		fmt.Sprintf("HIVE_NATS_PORT=%d", port),
	)

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("hivectl %s failed: %v\nstdout: %s\nstderr: %s",
			strings.Join(args, " "), err, stdout.String(), stderr.String())
	}

	return stdout.String()
}

// assertAgentStatusField parses the JSON output of "hivectl agents status" and
// asserts that the given field matches the expected value.
func assertAgentStatusField(t *testing.T, statusJSON, agentID, field string, expected interface{}) {
	t.Helper()

	var status map[string]interface{}
	if err := json.Unmarshal([]byte(statusJSON), &status); err != nil {
		t.Fatalf("parsing agent status JSON for %s: %v\nraw: %s", agentID, err, statusJSON)
	}

	got, ok := status[field]
	if !ok {
		t.Fatalf("agent %s status missing field %q; full status: %s", agentID, field, statusJSON)
	}

	if got != expected {
		t.Fatalf("agent %s: expected %s=%v (%T), got %v (%T)", agentID, field, expected, expected, got, got)
	}
}

func runHivectlValidateWithError(t *testing.T, binDir, clusterRoot string) (string, error) {
	t.Helper()

	cmd := exec.Command(filepath.Join(binDir, "hivectl"), "--cluster-root", clusterRoot, "validate")
	cmd.Env = append(os.Environ(), "HIVE_TEST_FIRECRACKER=mock")

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	return stdout.String() + stderr.String(), err
}
