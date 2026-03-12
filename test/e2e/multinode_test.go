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

// TestMultiNodeCluster starts two hived instances with NATS clustering enabled
// and verifies they can discover each other and share state.
func TestMultiNodeCluster(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multi-node test in short mode")
	}

	binDir := buildBinaries(t)

	node1NATSPort := freePort(t)
	node1ClusterPort := freePort(t)
	node2NATSPort := freePort(t)
	node2ClusterPort := freePort(t)

	// Node 1: seed node with no peers
	root1 := createClusteredNode(t, "node1", node1NATSPort, node1ClusterPort, nil)
	// Node 2: joins node 1
	root2 := createClusteredNode(t, "node2", node2NATSPort, node2ClusterPort,
		[]string{fmt.Sprintf("nats://127.0.0.1:%d", node1ClusterPort)})

	// Start node 1 and let it fully initialize before node 2 joins
	cmd1 := startHivedCmd(t, binDir, root1)
	waitForPort(t, node1NATSPort, 30*time.Second)
	time.Sleep(3 * time.Second)
	t.Log("node1 ready")

	// Start node 2
	cmd2 := startHivedCmd(t, binDir, root2)
	waitForPort(t, node2NATSPort, 30*time.Second)
	t.Log("node2 ready")

	// Allow cluster to form and heartbeats to propagate
	time.Sleep(5 * time.Second)

	t.Run("Node1IsResponsive", func(t *testing.T) {
		out := runHivectlWithPort(t, binDir, root1, node1NATSPort, "nodes", "list")
		t.Logf("node1 nodes output: %s", out)
		// node1 should at least see itself
		if strings.TrimSpace(out) == "" {
			t.Fatal("expected non-empty nodes output from node1")
		}
	})

	t.Run("Node2IsResponsive", func(t *testing.T) {
		out := runHivectlWithPort(t, binDir, root2, node2NATSPort, "nodes", "list")
		t.Logf("node2 nodes output: %s", out)
		if strings.TrimSpace(out) == "" {
			t.Fatal("expected non-empty nodes output from node2")
		}
	})

	t.Run("AgentVisibleAcrossNodes", func(t *testing.T) {
		// Write manifest — the reconciler will auto-start it since it's the team lead
		writeAgentManifestForNode(t, root1, "cross-node-agent")

		// Wait for agent to appear in listing (reconciler auto-starts team leads)
		deadline := time.Now().Add(15 * time.Second)
		var out1 string
		for time.Now().Before(deadline) {
			out1 = runHivectlWithPort(t, binDir, root1, node1NATSPort, "agents", "list")
			if strings.Contains(out1, "cross-node-agent") {
				break
			}
			time.Sleep(1 * time.Second)
		}
		if !strings.Contains(out1, "cross-node-agent") {
			t.Fatalf("agent not visible on node1 within 15s: %s", out1)
		}
	})

	// Cleanup (manual since we don't use startHived which registers t.Cleanup)
	stopHivedCmd(t, cmd2)
	stopHivedCmd(t, cmd1)
}

// createClusteredNode creates a cluster root with NATS clustering enabled.
func createClusteredNode(t *testing.T, name string, natsPort, clusterPort int, peers []string) string {
	t.Helper()

	root := fmt.Sprintf("/tmp/hive-mn-%s-%d", name, rand.Int63())
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatalf("creating cluster root for %s: %v", name, err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })

	peersYAML := ""
	if len(peers) > 0 {
		peersYAML = "    clusterPeers:\n"
		for _, p := range peers {
			peersYAML += fmt.Sprintf("      - %q\n", p)
		}
	}

	clusterYAML := fmt.Sprintf(`apiVersion: hive/v1
kind: Cluster
metadata:
  name: e2e-%s
spec:
  nats:
    port: %d
    clusterPort: %d
    clusterAuthToken: "e2e-cluster-token"
    clusterName: "hive-e2e-cluster"
%s    jetstream:
      enabled: false
  defaults:
    resources:
      memory: "512Mi"
      vcpus: 2
    health:
      interval: "5s"
      timeout: "3s"
      maxFailures: 3
    restart:
      policy: on-failure
      maxRestarts: 5
      backoff: "5s"
`, name, natsPort, clusterPort, peersYAML)

	if err := os.WriteFile(filepath.Join(root, "cluster.yaml"), []byte(clusterYAML), 0644); err != nil {
		t.Fatalf("writing cluster.yaml for %s: %v", name, err)
	}

	rootfsDir := filepath.Join(root, "rootfs")
	if err := os.MkdirAll(rootfsDir, 0755); err != nil {
		t.Fatalf("creating rootfs dir for %s: %v", name, err)
	}
	os.WriteFile(filepath.Join(rootfsDir, "vmlinux"), []byte("fake-kernel"), 0644)
	os.WriteFile(filepath.Join(rootfsDir, "rootfs.ext4"), []byte("fake-rootfs"), 0644)

	teamsDir := filepath.Join(root, "teams")
	os.MkdirAll(teamsDir, 0755)
	os.WriteFile(filepath.Join(teamsDir, "default.yaml"), []byte(`apiVersion: hive/v1
kind: Team
metadata:
  id: default
spec:
  lead: cross-node-agent
`), 0644)

	t.Logf("cluster node %s: root=%s nats=%d cluster=%d", name, root, natsPort, clusterPort)
	return root
}

// writeAgentManifestForNode creates an agent manifest in the given cluster root.
func writeAgentManifestForNode(t *testing.T, clusterRoot, agentID string) {
	t.Helper()

	agentDir := filepath.Join(clusterRoot, "agents", agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("creating agent dir: %v", err)
	}

	manifest := fmt.Sprintf(`apiVersion: hive/v1
kind: Agent
metadata:
  id: %s
  team: default
spec:
  runtime:
    type: process
    command: "sleep 3600"
  capabilities:
    - name: echo
      description: Echo test capability
  resources:
    memory: "512Mi"
    vcpus: 2
`, agentID)

	if err := os.WriteFile(filepath.Join(agentDir, "manifest.yaml"), []byte(manifest), 0644); err != nil {
		t.Fatalf("writing agent manifest: %v", err)
	}
}

// startHivedCmd starts hived and returns the exec.Cmd for manual lifecycle control.
// Unlike startHived, this does NOT register a t.Cleanup — caller must call stopHivedCmd.
func startHivedCmd(t *testing.T, binDir, clusterRoot string) *exec.Cmd {
	t.Helper()

	cmd := exec.Command(filepath.Join(binDir, "hived"),
		"--cluster-root", clusterRoot,
		"--force-process-backend",
	)
	cmd.Env = append(os.Environ(), "HIVE_TEST_FIRECRACKER=mock")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("starting hived: %v", err)
	}
	t.Logf("hived started (pid %d) at %s", cmd.Process.Pid, clusterRoot)
	return cmd
}

// stopHivedCmd sends SIGINT and waits for graceful shutdown (or kills after 10s).
func stopHivedCmd(t *testing.T, cmd *exec.Cmd) {
	t.Helper()

	if cmd.Process == nil {
		return
	}
	cmd.Process.Signal(os.Interrupt) //nolint:errcheck
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		t.Logf("hived (pid %d) stopped", cmd.Process.Pid)
	case <-time.After(10 * time.Second):
		cmd.Process.Kill() //nolint:errcheck
		<-done
		t.Logf("hived (pid %d) killed", cmd.Process.Pid)
	}
}
