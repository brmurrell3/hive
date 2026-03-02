// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// syncBuffer is a thread-safe bytes.Buffer for capturing subprocess output.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (sb *syncBuffer) Write(p []byte) (n int, err error) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.buf.Write(p)
}

func (sb *syncBuffer) String() string {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.buf.String()
}

// freePort returns an available TCP port by opening and immediately closing a listener.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// TestDemoPipeline validates the Phase 1 demo flow end-to-end:
// hivectl init --template ci-pipeline → hivectl dev → hivectl trigger → pipeline report.
func TestDemoPipeline(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping demo e2e test in short mode")
	}

	binDir := buildBinaries(t)

	// Use unique ports to avoid conflicts with other tests or leftover processes.
	natsPort := freePort(t)
	sidecarBase := freePort(t)
	callbackBase := sidecarBase + 100 // ensure separation

	clusterRoot := initDemoCluster(t, binDir, natsPort)

	// Start hivectl dev as a subprocess.
	devOutput := &syncBuffer{}
	startDevMode(t, binDir, clusterRoot, devOutput, sidecarBase, callbackBase)

	// Wait for all three agents to be healthy (look for "all agents started" in output).
	waitForDevReady(t, devOutput, 30*time.Second)

	// Wait a beat for Python HTTP servers to start.
	time.Sleep(2 * time.Second)

	// Trigger the pipeline. The trigger command subscribes for the result
	// and prints the JSON report directly to stdout.
	triggerOutput := runHivectl(t, binDir, clusterRoot, "trigger",
		"--team", "ci-pipeline",
		"--timeout", "30",
		"--payload", `{"repo_path": ".", "test_command": "echo PASS: all tests passed", "file_path": "README.md"}`,
	)

	var report map[string]interface{}
	if err := json.Unmarshal([]byte(triggerOutput), &report); err != nil {
		t.Fatalf("trigger output is not valid JSON: %v\noutput: %s", err, triggerOutput)
	}
	t.Logf("pipeline report:\n%s", prettyJSON(t, report))

	// Validate report structure.
	t.Run("report_has_pipeline_field", func(t *testing.T) {
		if report["pipeline"] != "ci-pipeline" {
			t.Fatalf("expected pipeline=ci-pipeline, got %v", report["pipeline"])
		}
	})

	t.Run("report_has_overall", func(t *testing.T) {
		overall, ok := report["overall"].(string)
		if !ok || (overall != "pass" && overall != "fail") {
			t.Fatalf("expected overall=pass|fail, got %v", report["overall"])
		}
	})

	t.Run("report_has_duration", func(t *testing.T) {
		dur, ok := report["duration_seconds"].(float64)
		if !ok || dur <= 0 {
			t.Fatalf("expected positive duration_seconds, got %v", report["duration_seconds"])
		}
	})

	t.Run("report_has_review", func(t *testing.T) {
		review, ok := report["review"].(map[string]interface{})
		if !ok {
			t.Fatalf("expected review object, got %v", report["review"])
		}
		if _, ok := review["review"]; !ok {
			t.Fatalf("expected review.review field, got %v", review)
		}
	})

	t.Run("report_has_tests", func(t *testing.T) {
		tests, ok := report["tests"].(map[string]interface{})
		if !ok {
			t.Fatalf("expected tests object, got %v", report["tests"])
		}
		if status, _ := tests["status"].(string); status != "success" {
			t.Fatalf("expected tests.status=success, got %v", tests)
		}
	})

	t.Run("report_has_security", func(t *testing.T) {
		security, ok := report["security"].(map[string]interface{})
		if !ok {
			t.Fatalf("expected security object, got %v", report["security"])
		}
		if status, _ := security["status"].(string); status != "success" {
			t.Fatalf("expected security.status=success, got %v", security)
		}
	})
}

// initDemoCluster runs hivectl init --template ci-pipeline in a temp directory,
// then patches cluster.yaml to use the given NATS port.
func initDemoCluster(t *testing.T, binDir string, natsPort int) string {
	t.Helper()

	// Use short path to avoid macOS socket limits.
	root := fmt.Sprintf("/tmp/hive-demo-%d", rand.Int63())
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatalf("creating demo root: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })

	cmd := exec.Command(filepath.Join(binDir, "hivectl"), "init", "--template", "ci-pipeline", "demo")
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("hivectl init failed: %v\nstderr: %s", err, stderr.String())
	}
	t.Logf("init output: %s", strings.TrimSpace(string(out)))

	// Patch cluster.yaml to use the unique NATS port.
	clusterYAML := filepath.Join(root, "demo", "cluster.yaml")
	data, err := os.ReadFile(clusterYAML)
	if err != nil {
		t.Fatalf("reading cluster.yaml: %v", err)
	}
	patched := strings.Replace(string(data), "port: 4222", fmt.Sprintf("port: %d", natsPort), 1)
	if err := os.WriteFile(clusterYAML, []byte(patched), 0644); err != nil {
		t.Fatalf("writing cluster.yaml: %v", err)
	}
	t.Logf("patched NATS port to %d", natsPort)

	return filepath.Join(root, "demo")
}

// startDevMode starts hivectl dev as a background process.
// All output (stdout+stderr) is written to the provided syncBuffer.
// The process group is killed during test cleanup.
func startDevMode(t *testing.T, binDir, clusterRoot string, output *syncBuffer, sidecarBase, callbackBase int) {
	t.Helper()

	cmd := exec.Command(filepath.Join(binDir, "hivectl"), "dev",
		"--cluster-root", clusterRoot,
		"--sidecar-port-base", fmt.Sprintf("%d", sidecarBase),
		"--callback-port-base", fmt.Sprintf("%d", callbackBase),
	)
	// Create a new process group so we can kill the entire tree (including Python children).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Use pipes instead of direct writer assignment to avoid blocking Wait().
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("creating stdout pipe: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("creating stderr pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("starting hivectl dev: %v", err)
	}
	t.Logf("hivectl dev started (pid %d)", cmd.Process.Pid)

	// Read output asynchronously.
	go io.Copy(output, stdout) //nolint:errcheck
	go io.Copy(output, stderr) //nolint:errcheck

	t.Cleanup(func() {
		// Kill the entire process group.
		pgid := cmd.Process.Pid
		syscall.Kill(-pgid, syscall.SIGKILL) //nolint:errcheck
		cmd.Wait()                           //nolint:errcheck
	})
}

// waitForDevReady polls the output buffer until "all agents started" appears.
func waitForDevReady(t *testing.T, output *syncBuffer, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(output.String(), "all agents started") {
			t.Log("dev mode ready: all agents started")
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("hivectl dev did not become ready within %s\noutput:\n%s", timeout, output.String())
}


func prettyJSON(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}
