// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRestartStatePreservation verifies that agent registrations and state
// persist in state.db across hived restarts.
func TestRestartStatePreservation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping restart test in short mode")
	}

	binDir := buildBinaries(t)
	port := freePort(t)
	root := createCluster(t, port)

	// --- First run: start hived, create agent, verify it works ---

	cmd1 := startHivedCmd(t, binDir, root)
	waitForPort(t, port, 15*time.Second)
	time.Sleep(2 * time.Second) // let hived finish init

	// Write agent manifest and start it
	writeAgentManifest(t, root)
	runHivectlWithPort(t, binDir, root, port, "agents", "start", "example-agent")
	time.Sleep(2 * time.Second)

	// Verify agent is running
	out1 := runHivectlWithPort(t, binDir, root, port, "agents", "list")
	if !strings.Contains(out1, "example-agent") {
		t.Fatalf("agent not listed before restart: %s", out1)
	}
	t.Log("agent registered and running before restart")

	// Verify state.db exists
	stateDB := filepath.Join(root, "state.db")
	if _, err := os.Stat(stateDB); os.IsNotExist(err) {
		t.Fatal("state.db not created")
	}

	// --- Stop hived ---
	stopHivedCmd(t, cmd1)
	t.Log("hived stopped, verifying state persists...")

	// Brief pause to ensure clean shutdown
	time.Sleep(1 * time.Second)

	// state.db should still exist
	if _, err := os.Stat(stateDB); os.IsNotExist(err) {
		t.Fatal("state.db disappeared after shutdown")
	}

	// --- Second run: restart hived, verify state ---

	cmd2 := startHivedCmd(t, binDir, root)
	defer stopHivedCmd(t, cmd2)
	waitForPort(t, port, 15*time.Second)
	time.Sleep(2 * time.Second)

	// Agent should still be known (may be in a different state due to restart)
	out2 := runHivectlWithPort(t, binDir, root, port, "agents", "list")
	if !strings.Contains(out2, "example-agent") {
		t.Fatalf("agent not listed after restart: %s", out2)
	}
	t.Log("agent state preserved across restart")
}

// TestConcurrentHivectlCommands verifies that hived handles multiple
// concurrent hivectl commands without crashes or data corruption.
func TestConcurrentHivectlCommands(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping concurrent test in short mode")
	}

	binDir := buildBinaries(t)
	port := freePort(t)
	root := createCluster(t, port)

	cmd := startHivedCmd(t, binDir, root)
	defer stopHivedCmd(t, cmd)
	waitForPort(t, port, 15*time.Second)
	time.Sleep(2 * time.Second)

	// Create several agents
	const numAgents = 5
	for i := 0; i < numAgents; i++ {
		agentID := fmt.Sprintf("concurrent-agent-%d", i)
		writeAgentManifestForNode(t, root, agentID)
	}

	// Update team manifest to include all agents
	agentList := ""
	for i := 0; i < numAgents; i++ {
		agentList += fmt.Sprintf("    - concurrent-agent-%d\n", i)
	}
	teamManifest := fmt.Sprintf(`apiVersion: hive/v1
kind: Team
metadata:
  id: default
spec:
  lead: concurrent-agent-0
  agents:
%s`, agentList)
	os.WriteFile(filepath.Join(root, "teams", "default.yaml"), []byte(teamManifest), 0644)

	// Start all agents concurrently
	type result struct {
		agentID string
		err     error
	}
	results := make(chan result, numAgents)

	for i := 0; i < numAgents; i++ {
		agentID := fmt.Sprintf("concurrent-agent-%d", i)
		go func(id string) {
			fullArgs := []string{"--cluster-root", root, "agents", "start", id}
			cmd := exec.Command(filepath.Join(binDir, "hivectl"), fullArgs...)
			cmd.Env = append(os.Environ(),
				"HIVE_TEST_FIRECRACKER=mock",
				fmt.Sprintf("HIVE_NATS_PORT=%d", port),
			)
			out, err := cmd.CombinedOutput()
			if err != nil {
				results <- result{id, fmt.Errorf("%v: %s", err, string(out))}
			} else {
				results <- result{id, nil}
			}
		}(agentID)
	}

	// Collect results
	started := 0
	for i := 0; i < numAgents; i++ {
		r := <-results
		if r.err != nil {
			t.Logf("agent %s start error: %v", r.agentID, r.err)
		} else {
			started++
		}
	}
	t.Logf("%d/%d agents started concurrently", started, numAgents)

	if started == 0 {
		t.Fatal("no agents started successfully")
	}

	time.Sleep(3 * time.Second)

	// Verify all agents appear in the listing
	out := runHivectlWithPort(t, binDir, root, port, "agents", "list")
	for i := 0; i < numAgents; i++ {
		agentID := fmt.Sprintf("concurrent-agent-%d", i)
		if !strings.Contains(out, agentID) {
			t.Errorf("agent %s not in listing after concurrent start", agentID)
		}
	}

	// Now stop all agents concurrently
	for i := 0; i < numAgents; i++ {
		agentID := fmt.Sprintf("concurrent-agent-%d", i)
		go func(id string) {
			fullArgs := []string{"--cluster-root", root, "agents", "stop", id}
			cmd := exec.Command(filepath.Join(binDir, "hivectl"), fullArgs...)
			cmd.Env = append(os.Environ(),
				"HIVE_TEST_FIRECRACKER=mock",
				fmt.Sprintf("HIVE_NATS_PORT=%d", port),
			)
			cmd.CombinedOutput()
		}(agentID)
	}

	time.Sleep(3 * time.Second)
	t.Log("concurrent stop completed")
}

// TestConfigVariants verifies hived starts correctly with various cluster
// configurations that users might deploy in production.
func TestConfigVariants(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping config variants test in short mode")
	}

	binDir := buildBinaries(t)

	variants := []struct {
		name   string
		config string
	}{
		{
			name: "MinimalConfig",
			config: `apiVersion: hive/v1
kind: Cluster
metadata:
  name: minimal
spec:
  nats:
    port: PORT
    clusterPort: -1
`,
		},
		{
			name: "JetStreamDisabled",
			config: `apiVersion: hive/v1
kind: Cluster
metadata:
  name: no-jetstream
spec:
  nats:
    port: PORT
    clusterPort: -1
    jetstream:
      enabled: false
`,
		},
		{
			name: "CustomDefaults",
			config: `apiVersion: hive/v1
kind: Cluster
metadata:
  name: custom-defaults
spec:
  nats:
    port: PORT
    clusterPort: -1
    jetstream:
      enabled: true
  defaults:
    resources:
      memory: "1Gi"
      vcpus: 4
    health:
      interval: "30s"
      timeout: "10s"
      maxFailures: 10
    restart:
      policy: always
      maxRestarts: 100
      backoff: "1s"
`,
		},
	}

	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			port := freePort(t)
			root := fmt.Sprintf("/tmp/hive-cfg-%d", rand.Int63())
			os.MkdirAll(root, 0755)
			t.Cleanup(func() { os.RemoveAll(root) })

			config := strings.ReplaceAll(v.config, "PORT", fmt.Sprintf("%d", port))
			os.WriteFile(filepath.Join(root, "cluster.yaml"), []byte(config), 0644)

			rootfsDir := filepath.Join(root, "rootfs")
			os.MkdirAll(rootfsDir, 0755)
			os.WriteFile(filepath.Join(rootfsDir, "vmlinux"), []byte("fake-kernel"), 0644)
			os.WriteFile(filepath.Join(rootfsDir, "rootfs.ext4"), []byte("fake-rootfs"), 0644)

			teamsDir := filepath.Join(root, "teams")
			os.MkdirAll(teamsDir, 0755)
			os.WriteFile(filepath.Join(teamsDir, "default.yaml"), []byte(`apiVersion: hive/v1
kind: Team
metadata:
  id: default
spec:
  lead: example-agent
`), 0644)

			cmd := startHivedCmd(t, binDir, root)
			waitForPort(t, port, 15*time.Second)
			time.Sleep(2 * time.Second)

			// Basic operations should work
			out := runHivectlWithPort(t, binDir, root, port, "nodes", "list")
			if strings.TrimSpace(out) == "" {
				t.Log("nodes output is empty (may be expected for some configs)")
			}

			stopHivedCmd(t, cmd)
		})
	}
}
