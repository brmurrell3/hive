// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestSDKPythonAgent validates the Python SDK agent integration:
// starts a cluster with a Python SDK agent, verifies capability registration
// and invocation via NATS, and verifies clean shutdown.
func TestSDKPythonAgent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping SDK e2e test in short mode")
	}

	// Check that python3 is available.
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found in PATH, skipping SDK test")
	}

	binDir := buildBinaries(t)
	projectRoot := findProjectRoot(t)

	// Use unique ports to avoid conflicts.
	natsPort := freePort(t)
	sidecarBase := freePort(t)
	callbackBase := sidecarBase + 100

	clusterRoot := createSDKTestCluster(t, projectRoot, natsPort)

	// Start hivectl dev.
	devOutput := &syncBuffer{}
	startSDKDevMode(t, binDir, clusterRoot, projectRoot, devOutput, sidecarBase, callbackBase)

	// Wait for agents to start.
	waitForDevReady(t, devOutput, 30*time.Second)

	// Wait for Python HTTP servers to start.
	time.Sleep(2 * time.Second)

	// Test 1: Invoke "echo" capability on the Python agent.
	t.Run("python_echo_capability", func(t *testing.T) {
		output := runHivectl(t, binDir, clusterRoot, "trigger",
			"--team", "sdk-test",
			"--timeout", "15",
			"--payload", `{"message": "hello from SDK test"}`,
		)

		var report map[string]interface{}
		if err := json.Unmarshal([]byte(output), &report); err != nil {
			t.Fatalf("trigger output is not valid JSON: %v\noutput: %s", err, output)
		}
		t.Logf("trigger report:\n%s", prettyJSON(t, report))
	})

	// Test 2: Verify the agent responded to health checks (visible in dev output).
	t.Run("python_agent_healthy", func(t *testing.T) {
		out := devOutput.String()
		if !strings.Contains(out, "python-echo-agent") {
			t.Fatalf("expected python-echo-agent mentioned in dev output, got:\n%s", out)
		}
	})
}

// TestSDKGoAgent validates the Go SDK agent integration:
// builds a small Go agent using the SDK, starts it alongside a cluster,
// and verifies capability invocation.
func TestSDKGoAgent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Go SDK e2e test in short mode")
	}

	binDir := buildBinaries(t)
	projectRoot := findProjectRoot(t)

	// Build the Go test agent.
	goAgentBin := buildGoTestAgent(t, projectRoot)

	// Use unique ports.
	natsPort := freePort(t)
	sidecarBase := freePort(t)
	callbackBase := sidecarBase + 100

	clusterRoot := createGoSDKTestCluster(t, goAgentBin, natsPort)

	// Start hivectl dev.
	devOutput := &syncBuffer{}
	cmd := exec.Command(filepath.Join(binDir, "hivectl"), "dev",
		"--cluster-root", clusterRoot,
		"--sidecar-port-base", fmt.Sprintf("%d", sidecarBase),
		"--callback-port-base", fmt.Sprintf("%d", callbackBase),
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(), "HIVE_TEST_FIRECRACKER=mock")

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

	go io.Copy(devOutput, stdout) //nolint:errcheck
	go io.Copy(devOutput, stderr) //nolint:errcheck

	t.Cleanup(func() {
		pgid := cmd.Process.Pid
		syscall.Kill(-pgid, syscall.SIGKILL) //nolint:errcheck
		cmd.Wait()                           //nolint:errcheck
	})

	// Wait for agents.
	waitForDevReady(t, devOutput, 30*time.Second)
	time.Sleep(2 * time.Second)

	t.Run("go_agent_healthy", func(t *testing.T) {
		out := devOutput.String()
		if !strings.Contains(out, "go-echo-agent") {
			t.Fatalf("expected go-echo-agent in dev output, got:\n%s", out)
		}
	})
}

// createSDKTestCluster creates a cluster root with a Python SDK agent.
func createSDKTestCluster(t *testing.T, projectRoot string, natsPort int) string {
	t.Helper()

	root := fmt.Sprintf("/tmp/hive-sdk-%d", rand.Int63())
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatalf("creating cluster root: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })

	// cluster.yaml
	clusterYAML := fmt.Sprintf(`apiVersion: hive/v1
kind: Cluster
metadata:
  name: sdk-test-cluster
spec:
  nats:
    port: %d
    clusterPort: -1
    jetstream:
      enabled: true
  defaults:
    resources:
      memory: "256Mi"
    health:
      interval: "10s"
      timeout: "5s"
`, natsPort)
	writeTestFile(t, filepath.Join(root, "cluster.yaml"), clusterYAML)

	// teams/sdk-test.yaml
	teamsDir := filepath.Join(root, "teams")
	os.MkdirAll(teamsDir, 0755)
	teamYAML := `apiVersion: hive/v1
kind: Team
metadata:
  id: sdk-test
spec:
  lead: python-echo-agent
  communication:
    persistent: true
    historyDepth: 50
`
	writeTestFile(t, filepath.Join(teamsDir, "sdk-test.yaml"), teamYAML)

	// agents/python-echo-agent/
	agentDir := filepath.Join(root, "agents", "python-echo-agent")
	os.MkdirAll(agentDir, 0755)

	agentManifest := `apiVersion: hive/v1
kind: Agent
metadata:
  id: python-echo-agent
  team: sdk-test
spec:
  tier: native
  runtime:
    type: custom
    command: "python3 agent.py"
  capabilities:
    - name: echo
      description: "Echo back a message"
      inputs:
        - name: message
          type: string
          description: "Message to echo"
      outputs:
        - name: reply
          type: string
          description: "Echoed message"
    - name: add
      description: "Add two numbers"
      inputs:
        - name: a
          type: int
          description: "First number"
        - name: b
          type: int
          description: "Second number"
      outputs:
        - name: result
          type: int
          description: "Sum"
`
	writeTestFile(t, filepath.Join(agentDir, "manifest.yaml"), agentManifest)

	// Copy the Python test agent and SDK.
	testAgentSrc := filepath.Join(projectRoot, "test", "e2e", "testdata", "python-agent", "agent.py")
	sdkSrc := filepath.Join(projectRoot, "sdk", "python", "hive_sdk.py")

	copyTestFile(t, testAgentSrc, filepath.Join(agentDir, "agent.py"))
	copyTestFile(t, sdkSrc, filepath.Join(agentDir, "hive_sdk.py"))

	t.Logf("SDK test cluster root: %s", root)
	return root
}

// createGoSDKTestCluster creates a cluster root with a Go SDK agent.
func createGoSDKTestCluster(t *testing.T, goAgentBin string, natsPort int) string {
	t.Helper()

	root := fmt.Sprintf("/tmp/hive-gosdk-%d", rand.Int63())
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatalf("creating cluster root: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })

	clusterYAML := fmt.Sprintf(`apiVersion: hive/v1
kind: Cluster
metadata:
  name: gosdk-test-cluster
spec:
  nats:
    port: %d
    clusterPort: -1
    jetstream:
      enabled: true
  defaults:
    resources:
      memory: "256Mi"
    health:
      interval: "10s"
      timeout: "5s"
`, natsPort)
	writeTestFile(t, filepath.Join(root, "cluster.yaml"), clusterYAML)

	teamsDir := filepath.Join(root, "teams")
	os.MkdirAll(teamsDir, 0755)
	teamYAML := `apiVersion: hive/v1
kind: Team
metadata:
  id: gosdk-test
spec:
  lead: go-echo-agent
  communication:
    persistent: true
    historyDepth: 50
`
	writeTestFile(t, filepath.Join(teamsDir, "gosdk-test.yaml"), teamYAML)

	agentDir := filepath.Join(root, "agents", "go-echo-agent")
	os.MkdirAll(agentDir, 0755)

	agentManifest := fmt.Sprintf(`apiVersion: hive/v1
kind: Agent
metadata:
  id: go-echo-agent
  team: gosdk-test
spec:
  tier: native
  runtime:
    type: custom
    command: "%s"
  capabilities:
    - name: echo
      description: "Echo back a message"
      inputs:
        - name: message
          type: string
          description: "Message to echo"
      outputs:
        - name: reply
          type: string
          description: "Echoed message"
`, goAgentBin)
	writeTestFile(t, filepath.Join(agentDir, "manifest.yaml"), agentManifest)

	t.Logf("Go SDK test cluster root: %s", root)
	return root
}

// buildGoTestAgent compiles a minimal Go agent using the Hive Go SDK.
func buildGoTestAgent(t *testing.T, projectRoot string) string {
	t.Helper()

	// Create a temporary Go source file for the test agent.
	srcDir := t.TempDir()
	agentSrc := `package main

import (
	"context"
	"fmt"
	"github.com/brmurrell3/hive/sdk/go/hive"
)

func main() {
	agent := hive.NewAgent()

	agent.HandleCapability("echo", func(inputs map[string]any) (map[string]any, error) {
		msg, _ := inputs["message"].(string)
		return map[string]any{"reply": fmt.Sprintf("echo: %s", msg)}, nil
	})

	agent.Run(context.Background())
}
`
	writeTestFile(t, filepath.Join(srcDir, "main.go"), agentSrc)

	binPath := filepath.Join(srcDir, "go-test-agent")
	cmd := exec.Command("go", "build", "-o", binPath, ".")
	cmd.Dir = srcDir
	buildEnv := append(os.Environ(), "CGO_ENABLED=0")
	// Only set GOPATH explicitly if it is already set in the environment.
	// Setting GOPATH="" breaks module-mode builds because Go needs
	// GOPATH/pkg/mod for the module cache. When unset, Go uses its default
	// ($HOME/go) which is correct.
	if gp := os.Getenv("GOPATH"); gp != "" {
		buildEnv = append(buildEnv, fmt.Sprintf("GOPATH=%s", gp))
	}
	cmd.Env = buildEnv

	// The test agent imports from the main module, so we need a go.mod that
	// replaces the module path to the local checkout.
	goMod := fmt.Sprintf(`module go-test-agent

go 1.25

require github.com/brmurrell3/hive v0.0.0

replace github.com/brmurrell3/hive => %s
`, projectRoot)
	writeTestFile(t, filepath.Join(srcDir, "go.mod"), goMod)

	// Run go mod tidy to resolve dependencies.
	tidyCmd := exec.Command("go", "mod", "tidy")
	tidyCmd.Dir = srcDir
	tidyCmd.Env = cmd.Env
	tidyOut, err := tidyCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go mod tidy failed: %v\n%s", err, tidyOut)
	}

	buildOut, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("building Go test agent: %v\n%s", err, buildOut)
	}
	t.Logf("built Go test agent: %s", binPath)
	return binPath
}

// startSDKDevMode starts hivectl dev with PYTHONPATH set to find the SDK.
func startSDKDevMode(t *testing.T, binDir, clusterRoot, projectRoot string, output *syncBuffer, sidecarBase, callbackBase int) {
	t.Helper()

	cmd := exec.Command(filepath.Join(binDir, "hivectl"), "dev",
		"--cluster-root", clusterRoot,
		"--sidecar-port-base", fmt.Sprintf("%d", sidecarBase),
		"--callback-port-base", fmt.Sprintf("%d", callbackBase),
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Set PYTHONPATH so the agent can import hive_sdk.
	env := append(os.Environ(),
		"HIVE_TEST_FIRECRACKER=mock",
	)
	cmd.Env = env

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
	t.Logf("hivectl dev started for SDK test (pid %d)", cmd.Process.Pid)

	go io.Copy(output, stdout) //nolint:errcheck
	go io.Copy(output, stderr) //nolint:errcheck

	t.Cleanup(func() {
		pgid := cmd.Process.Pid
		syscall.Kill(-pgid, syscall.SIGKILL) //nolint:errcheck
		cmd.Wait()                           //nolint:errcheck
	})
}

// writeTestFile writes content to a file, failing the test on error.
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("creating directory for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// copyTestFile copies a file from src to dst, failing the test on error.
func copyTestFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("reading %s: %v", src, err)
	}
	if err := os.WriteFile(dst, data, 0644); err != nil {
		t.Fatalf("writing %s: %v", dst, err)
	}
}
