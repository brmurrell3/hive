// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build e2e

package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

const (
	natsPort = 14222
)

// TestHiveE2E runs the full end-to-end lifecycle test.
// It builds the real binaries, starts hived as a subprocess with mock Firecracker,
// then exercises hivectl commands, agent join, capability routing, and health
// heartbeats — all without hardware or Docker.
func TestHiveE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}

	binDir := buildBinaries(t)
	clusterRoot := createCluster(t)

	// Pre-seed a join token in state.db before hived starts.
	rawToken := "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	preSeedToken(t, clusterRoot, rawToken)

	stopHived := startHived(t, binDir, clusterRoot)
	defer stopHived()

	// Wait for hived to be ready (writes .state/nats-auth-token on startup).
	natsAuthTokenPath := filepath.Join(clusterRoot, ".state", "nats-auth-token")
	waitForFile(t, natsAuthTokenPath, 15*time.Second)

	// Read the NATS auth token that hived generated.
	natsAuthToken := readFile(t, natsAuthTokenPath)

	// Connect a NATS client for test verification.
	natsURL := fmt.Sprintf("nats://127.0.0.1:%d", natsPort)
	nc, err := nats.Connect(natsURL, nats.Token(natsAuthToken), nats.Timeout(10*time.Second))
	if err != nil {
		t.Fatalf("connecting to NATS: %v", err)
	}
	defer nc.Close()

	// --- Phase 1: hivectl agents list (empty — no manifests exist yet) ---
	t.Run("agents_list_empty", func(t *testing.T) {
		out := runHivectl(t, binDir, clusterRoot, "agents", "list")
		if !strings.Contains(out, "AGENT_ID") {
			t.Fatalf("expected header in agents list output, got: %s", out)
		}
		lines := nonEmptyLines(out)
		if len(lines) > 1 {
			t.Fatalf("expected only header line, got %d lines:\n%s", len(lines), out)
		}
	})

	// Now create the agent manifest so hivectl agents start can find it.
	writeAgentManifest(t, clusterRoot)

	// --- Phase 2: hivectl agents start example-agent ---
	t.Run("agents_start", func(t *testing.T) {
		out := runHivectl(t, binDir, clusterRoot, "agents", "start", "example-agent")
		if !strings.Contains(out, "started") {
			t.Fatalf("expected 'started' in output, got: %s", out)
		}
	})

	// --- Phase 3: hivectl agents status example-agent → RUNNING ---
	t.Run("agents_status_running", func(t *testing.T) {
		var out string
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			out = runHivectl(t, binDir, clusterRoot, "agents", "status", "example-agent")
			if strings.Contains(out, "RUNNING") {
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Fatalf("agent did not reach RUNNING state, last: %s", out)
	})

	// --- Phase 4: hivectl agents stop example-agent ---
	t.Run("agents_stop", func(t *testing.T) {
		out := runHivectl(t, binDir, clusterRoot, "agents", "stop", "example-agent")
		if !strings.Contains(out, "stopped") {
			t.Fatalf("expected 'stopped' in output, got: %s", out)
		}
	})

	// --- Phase 5: hivectl agents destroy example-agent ---
	t.Run("agents_destroy", func(t *testing.T) {
		out := runHivectl(t, binDir, clusterRoot, "agents", "destroy", "example-agent")
		if !strings.Contains(out, "destroyed") {
			t.Fatalf("expected 'destroyed' in output, got: %s", out)
		}
	})

	// --- Phase 6: Verify agent is gone ---
	t.Run("agents_list_after_destroy", func(t *testing.T) {
		out := runHivectl(t, binDir, clusterRoot, "agents", "list")
		if strings.Contains(out, "example-agent") {
			t.Fatalf("expected example-agent to be gone, got: %s", out)
		}
	})

	// --- Phase 7: Tier 2 native agent join (with manifest for capabilities) ---
	// Create a manifest for the native agent so it registers capabilities.
	nativeManifestPath := writeNativeAgentManifest(t)

	// Start agent at the parent level so it stays alive for subsequent subtests.
	agentCmd := exec.Command(
		filepath.Join(binDir, "hive-agent"), "join",
		"--token", rawToken,
		"--control-plane", fmt.Sprintf("127.0.0.1:%d", natsPort),
		"--agent-id", "native-agent-01",
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
		agentCmd.Wait()
	})

	// Give the agent time to complete the join handshake and start the sidecar.
	t.Run("agent_join", func(t *testing.T) {
		// Wait for the sidecar's heartbeat publisher to start — poll the health subject.
		sub, err := nc.SubscribeSync("hive.health.native-agent-01")
		if err != nil {
			t.Fatalf("subscribing: %v", err)
		}
		defer sub.Unsubscribe()
		nc.Flush()

		// The sidecar publishes an initial heartbeat shortly after start.
		// The health interval is 5s in our config, but the sidecar uses its own default (30s)
		// unless overridden. Wait up to 35s for the first heartbeat.
		msg, err := sub.NextMsg(35 * time.Second)
		if err != nil {
			t.Fatalf("waiting for first heartbeat from native-agent-01: %v", err)
		}
		t.Logf("agent joined and heartbeat received: %s", string(msg.Data))
	})

	// --- Phase 8: Health heartbeats ---
	// Already verified in agent_join above (initial heartbeat on the specific subject).
	// This test verifies the wildcard subscription pattern works too.
	t.Run("health_heartbeats", func(t *testing.T) {
		sub, err := nc.SubscribeSync("hive.health.>")
		if err != nil {
			t.Fatalf("subscribing to hive.health.>: %v", err)
		}
		defer sub.Unsubscribe()
		nc.Flush()

		// The native agent's heartbeat interval is 30s. We already consumed
		// the initial heartbeat in agent_join, so wait for the next one.
		msg, err := sub.NextMsg(35 * time.Second)
		if err != nil {
			t.Fatalf("timed out waiting for health heartbeat: %v", err)
		}
		t.Logf("heartbeat on %s: %s", msg.Subject, string(msg.Data))
	})

	// --- Phase 9: Capability routing (real sidecar handler) ---
	t.Run("capability_routing", func(t *testing.T) {
		// The native agent registered "summarize" via --manifest, so the
		// sidecar's capability router is subscribed to
		// hive.capabilities.native-agent-01.summarize.request
		// We send a properly formatted Envelope wrapping an InvocationRequest.
		capSubject := "hive.capabilities.native-agent-01.summarize.request"

		envelope := map[string]interface{}{
			"id":        "test-req-001",
			"from":      "e2e-test",
			"to":        "native-agent-01",
			"type":      "capability-request",
			"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
			"payload": map[string]interface{}{
				"capability": "summarize",
				"inputs":     map[string]interface{}{"text": "hello world"},
				"timeout":    "10s",
			},
		}
		reqData, err := json.Marshal(envelope)
		if err != nil {
			t.Fatalf("marshaling capability request envelope: %v", err)
		}

		msg, err := nc.Request(capSubject, reqData, 10*time.Second)
		if err != nil {
			t.Fatalf("capability request failed: %v", err)
		}

		// The response is an Envelope wrapping an InvocationResponse.
		var respEnv map[string]interface{}
		if err := json.Unmarshal(msg.Data, &respEnv); err != nil {
			t.Fatalf("parsing capability response envelope: %v", err)
		}

		payload, ok := respEnv["payload"].(map[string]interface{})
		if !ok {
			t.Fatalf("expected payload object in response, got: %v", respEnv)
		}

		status, _ := payload["status"].(string)
		if status != "success" {
			t.Fatalf("expected status=success, got: %v", payload)
		}

		// The no-op sidecar handler echoes back with status "executed".
		outputs, _ := payload["outputs"].(map[string]interface{})
		if outputs == nil {
			t.Fatalf("expected outputs in response, got: %v", payload)
		}
		if execStatus, _ := outputs["status"].(string); execStatus != "executed" {
			t.Fatalf("expected outputs.status=executed, got: %v", outputs)
		}
		t.Logf("capability routing via real sidecar: %v", payload)
	})

}

// buildBinaries compiles hived, hivectl, and hive-agent to a temp directory.
func buildBinaries(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	projectRoot := findProjectRoot(t)

	for _, bin := range []struct {
		name string
		pkg  string
	}{
		{"hived", "./cmd/hived"},
		{"hivectl", "./cmd/hivectl"},
		{"hive-agent", "./cmd/hive-agent"},
	} {
		outPath := filepath.Join(binDir, bin.name)
		cmd := exec.Command("go", "build", "-o", outPath, bin.pkg)
		cmd.Dir = projectRoot
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("building %s: %v\n%s", bin.name, err, stderr.String())
		}
		t.Logf("built %s -> %s", bin.name, outPath)
	}

	return binDir
}

// createCluster creates a cluster root with cluster.yaml and teams but NO agent
// manifests yet (so the reconciler doesn't auto-create agents before we test the
// empty list). Agent manifests are added later via writeAgentManifest.
//
// Uses a short path under /tmp to avoid exceeding Unix socket path limits on macOS.
func createCluster(t *testing.T) string {
	t.Helper()

	// Use a short path to avoid exceeding macOS's 104-byte Unix socket limit.
	root := fmt.Sprintf("/tmp/hive-e2e-%d", rand.Int63())
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatalf("creating cluster root: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })

	clusterYAML := fmt.Sprintf(`apiVersion: hive/v1
kind: Cluster
metadata:
  name: e2e-test-cluster
spec:
  nats:
    port: %d
    clusterPort: -1
    jetstream:
      enabled: true
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
`, natsPort)

	if err := os.WriteFile(filepath.Join(root, "cluster.yaml"), []byte(clusterYAML), 0644); err != nil {
		t.Fatalf("writing cluster.yaml: %v", err)
	}

	// rootfs/ — dummy kernel and rootfs for the mock hypervisor.
	// The VM manager copies these per-agent even in mock mode.
	rootfsDir := filepath.Join(root, "rootfs")
	if err := os.MkdirAll(rootfsDir, 0755); err != nil {
		t.Fatalf("creating rootfs dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootfsDir, "vmlinux"), []byte("fake-kernel"), 0644); err != nil {
		t.Fatalf("writing fake kernel: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootfsDir, "rootfs.ext4"), []byte("fake-rootfs"), 0644); err != nil {
		t.Fatalf("writing fake rootfs: %v", err)
	}

	// teams/default.yaml (needed for cross-team router)
	teamsDir := filepath.Join(root, "teams")
	if err := os.MkdirAll(teamsDir, 0755); err != nil {
		t.Fatalf("creating teams dir: %v", err)
	}
	teamManifest := `apiVersion: hive/v1
kind: Team
metadata:
  id: default
spec:
  lead: example-agent
`
	if err := os.WriteFile(filepath.Join(teamsDir, "default.yaml"), []byte(teamManifest), 0644); err != nil {
		t.Fatalf("writing team manifest: %v", err)
	}

	t.Logf("cluster root: %s", root)
	return root
}

// writeNativeAgentManifest creates a manifest YAML for the native agent with
// capabilities, returning the path. Used with --manifest on hive-agent join.
func writeNativeAgentManifest(t *testing.T) string {
	t.Helper()

	manifest := `apiVersion: hive/v1
kind: Agent
metadata:
  id: native-agent-01
  team: default
spec:
  tier: native
  runtime:
    type: noop
  capabilities:
    - name: summarize
      description: Summarizes input text
      inputs:
        - name: text
          type: string
          description: The text to summarize
      outputs:
        - name: summary
          type: string
          description: The summary
`
	path := filepath.Join(t.TempDir(), "native-agent-manifest.yaml")
	if err := os.WriteFile(path, []byte(manifest), 0644); err != nil {
		t.Fatalf("writing native agent manifest: %v", err)
	}
	return path
}

// writeAgentManifest creates the example-agent manifest in the cluster root.
// Called after the "empty list" test so the reconciler doesn't auto-create it.
func writeAgentManifest(t *testing.T, clusterRoot string) {
	t.Helper()

	agentDir := filepath.Join(clusterRoot, "agents", "example-agent")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("creating agent dir: %v", err)
	}

	manifest := `apiVersion: hive/v1
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
	if err := os.WriteFile(filepath.Join(agentDir, "manifest.yaml"), []byte(manifest), 0644); err != nil {
		t.Fatalf("writing agent manifest: %v", err)
	}
}

// preSeedToken writes a join token directly to state.db before hived starts.
func preSeedToken(t *testing.T, clusterRoot, rawToken string) {
	t.Helper()

	h := sha256.Sum256([]byte(rawToken))
	hash := hex.EncodeToString(h[:])
	prefix := rawToken[:8]

	tokenData := map[string]interface{}{
		"prefix":     prefix,
		"hash":       hash,
		"created_at": time.Now().UTC().Format(time.RFC3339),
		"expires_at": time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339),
		"revoked":    false,
	}
	dataJSON, err := json.Marshal(tokenData)
	if err != nil {
		t.Fatalf("marshaling token data: %v", err)
	}

	// Build a small helper program that creates state.db with the token.
	// This avoids importing internal packages from test/e2e.
	helperDir := t.TempDir()
	helperSrc := fmt.Sprintf(`package main

import (
	"database/sql"
	"fmt"
	"os"
	_ "modernc.org/sqlite"
)

func main() {
	dbPath := os.Args[1]
	hash := os.Args[2]
	dataJSON := os.Args[3]

	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)")
	if err != nil {
		fmt.Fprintf(os.Stderr, "open: %%v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	for _, ddl := range []string{
		"CREATE TABLE IF NOT EXISTS tokens (hash TEXT PRIMARY KEY, data_json TEXT NOT NULL)",
		"CREATE TABLE IF NOT EXISTS agents (id TEXT PRIMARY KEY, team TEXT, status TEXT, node_id TEXT, memory_bytes INTEGER, vcpus INTEGER, manifest_hash TEXT, spec_hash TEXT NOT NULL DEFAULT '', vm_pid INTEGER, vm_cid INTEGER, vm_socket_path TEXT, rootfs_copy_path TEXT, restart_count INTEGER DEFAULT 0, last_transition TEXT, started_at TEXT, error TEXT)",
		"CREATE TABLE IF NOT EXISTS nodes (id TEXT PRIMARY KEY, data_json TEXT NOT NULL)",
		"CREATE TABLE IF NOT EXISTS capabilities (agent_id TEXT PRIMARY KEY, data_json TEXT NOT NULL)",
		"CREATE TABLE IF NOT EXISTS users (id TEXT PRIMARY KEY, data_json TEXT NOT NULL)",
	} {
		if _, err := db.Exec(ddl); err != nil {
			fmt.Fprintf(os.Stderr, "ddl: %%v\n", err)
			os.Exit(1)
		}
	}

	_, err = db.Exec("INSERT OR REPLACE INTO tokens (hash, data_json) VALUES (?, ?)", hash, dataJSON)
	if err != nil {
		fmt.Fprintf(os.Stderr, "insert: %%v\n", err)
		os.Exit(1)
	}
	fmt.Println("token seeded")
}
`)
	helperPath := filepath.Join(helperDir, "seed.go")
	if err := os.WriteFile(helperPath, []byte(helperSrc), 0644); err != nil {
		t.Fatalf("writing seed helper: %v", err)
	}

	helperBin := filepath.Join(helperDir, "seed")
	projectRoot := findProjectRoot(t)

	buildCmd := exec.Command("go", "build", "-o", helperBin, helperPath)
	buildCmd.Dir = projectRoot
	var buildStderr bytes.Buffer
	buildCmd.Stderr = &buildStderr
	if err := buildCmd.Run(); err != nil {
		t.Fatalf("building seed helper: %v\n%s", err, buildStderr.String())
	}

	dbPath := filepath.Join(clusterRoot, "state.db")
	runCmd := exec.Command(helperBin, dbPath, hash, string(dataJSON))
	var runStderr bytes.Buffer
	runCmd.Stderr = &runStderr
	out, err := runCmd.Output()
	if err != nil {
		t.Fatalf("running seed helper: %v\n%s", err, runStderr.String())
	}
	t.Logf("token seeded: %s", strings.TrimSpace(string(out)))
}

// startHived starts hived as a subprocess with HIVE_TEST_FIRECRACKER=mock.
func startHived(t *testing.T, binDir, clusterRoot string) func() {
	t.Helper()

	cmd := exec.Command(filepath.Join(binDir, "hived"), "--cluster-root", clusterRoot)
	cmd.Env = append(os.Environ(), "HIVE_TEST_FIRECRACKER=mock")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("starting hived: %v", err)
	}
	t.Logf("hived started (pid %d)", cmd.Process.Pid)

	cleanup := func() {
		cmd.Process.Signal(os.Interrupt)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			cmd.Process.Kill()
			<-done
		}
	}

	t.Cleanup(cleanup)
	return cleanup
}

// runHivectl runs a hivectl command and returns stdout.
func runHivectl(t *testing.T, binDir, clusterRoot string, args ...string) string {
	t.Helper()

	fullArgs := append([]string{"--cluster-root", clusterRoot}, args...)
	cmd := exec.Command(filepath.Join(binDir, "hivectl"), fullArgs...)
	cmd.Env = append(os.Environ(), "HIVE_TEST_FIRECRACKER=mock")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("hivectl %s failed: %v\nstdout: %s\nstderr: %s",
			strings.Join(args, " "), err, stdout.String(), stderr.String())
	}

	return stdout.String()
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("file %s did not appear within %s", path, timeout)
}

func waitForPort(t *testing.T, port int, timeout time.Duration) {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("port %d did not become available within %s", port, timeout)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return strings.TrimSpace(string(data))
}

func findProjectRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find project root (go.mod)")
		}
		dir = parent
	}
}

func nonEmptyLines(s string) []string {
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
