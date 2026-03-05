// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build unit

package process

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/brmurrell3/hive/internal/types"
)

func testOpenClawManifest(id string) *types.AgentManifest {
	return &types.AgentManifest{
		APIVersion: "hive/v1",
		Kind:       "Agent",
		Metadata:   types.AgentMetadata{ID: id, Team: "test-team"},
		Spec: types.AgentSpec{
			Runtime: types.AgentRuntime{
				Type: "openclaw",
				Model: types.AgentModel{
					Provider: "anthropic",
					Name:     "claude-sonnet-4-5-20250514",
					Env: map[string]string{
						"ANTHROPIC_API_KEY": "sk-test-key",
					},
				},
			},
		},
	}
}

func testNonOpenClawManifest(id string) *types.AgentManifest {
	return &types.AgentManifest{
		APIVersion: "hive/v1",
		Kind:       "Agent",
		Metadata:   types.AgentMetadata{ID: id, Team: "test-team"},
		Spec: types.AgentSpec{
			Runtime: types.AgentRuntime{
				Type:    "custom",
				Command: "echo hello",
			},
		},
	}
}

func TestIsOpenClawRuntime(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		spec *types.AgentManifest
		want bool
	}{
		{
			name: "openclaw runtime",
			spec: testOpenClawManifest("agent-1"),
			want: true,
		},
		{
			name: "custom runtime",
			spec: testNonOpenClawManifest("agent-2"),
			want: false,
		},
		{
			name: "process runtime",
			spec: &types.AgentManifest{
				Spec: types.AgentSpec{
					Runtime: types.AgentRuntime{Type: "process"},
				},
			},
			want: false,
		},
		{
			name: "empty runtime type",
			spec: &types.AgentManifest{
				Spec: types.AgentSpec{
					Runtime: types.AgentRuntime{},
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := isOpenClawRuntime(tt.spec)
			if got != tt.want {
				t.Errorf("isOpenClawRuntime() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFindOpenClawBinary_NotInPath(t *testing.T) {
	// Cannot use t.Parallel with t.Setenv.
	t.Setenv("PATH", "")

	_, err := findOpenClawBinary()
	if err == nil {
		t.Fatal("findOpenClawBinary: expected error when binary not in PATH, got nil")
	}
}

func TestGenerateOpenClawConfig(t *testing.T) {
	t.Parallel()

	spec := testOpenClawManifest("agent-1")
	workspacePath := "/tmp/test-workspace"
	port := 9300

	data, err := generateOpenClawConfig(spec, workspacePath, port)
	if err != nil {
		t.Fatalf("generateOpenClawConfig: %v", err)
	}

	var cfg openClawConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if cfg.Model.Provider != "anthropic" {
		t.Errorf("Model.Provider = %q, want %q", cfg.Model.Provider, "anthropic")
	}
	if cfg.Model.Name != "claude-sonnet-4-5-20250514" {
		t.Errorf("Model.Name = %q, want %q", cfg.Model.Name, "claude-sonnet-4-5-20250514")
	}
	if cfg.Model.Env["ANTHROPIC_API_KEY"] != "sk-test-key" {
		t.Errorf("Model.Env[ANTHROPIC_API_KEY] = %q, want %q", cfg.Model.Env["ANTHROPIC_API_KEY"], "sk-test-key")
	}
	if cfg.Gateway.Port != 9300 {
		t.Errorf("Gateway.Port = %d, want %d", cfg.Gateway.Port, 9300)
	}
	if cfg.Gateway.Host != "127.0.0.1" {
		t.Errorf("Gateway.Host = %q, want %q", cfg.Gateway.Host, "127.0.0.1")
	}
	if cfg.Workspace != workspacePath {
		t.Errorf("Workspace = %q, want %q", cfg.Workspace, workspacePath)
	}
}

func TestGenerateOpenClawConfig_EmptyModel(t *testing.T) {
	t.Parallel()

	spec := &types.AgentManifest{
		Spec: types.AgentSpec{
			Runtime: types.AgentRuntime{
				Type: "openclaw",
			},
		},
	}

	data, err := generateOpenClawConfig(spec, "/tmp/ws", 9301)
	if err != nil {
		t.Fatalf("generateOpenClawConfig: %v", err)
	}

	var cfg openClawConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if cfg.Model.Provider != "" {
		t.Errorf("Model.Provider = %q, want empty", cfg.Model.Provider)
	}
	if cfg.Model.Name != "" {
		t.Errorf("Model.Name = %q, want empty", cfg.Model.Name)
	}
}

func TestNextGatewayPort_Sequential(t *testing.T) {
	// This test modifies global state so it must not run in parallel
	// with other tests that use the port allocator.
	resetGatewayPort()
	defer resetGatewayPort()

	port1, err := nextGatewayPort()
	if err != nil {
		t.Fatalf("nextGatewayPort() returned unexpected error: %v", err)
	}
	port2, err := nextGatewayPort()
	if err != nil {
		t.Fatalf("nextGatewayPort() returned unexpected error: %v", err)
	}
	port3, err := nextGatewayPort()
	if err != nil {
		t.Fatalf("nextGatewayPort() returned unexpected error: %v", err)
	}

	if port1 != openClawBasePort {
		t.Errorf("first port = %d, want %d", port1, openClawBasePort)
	}
	if port2 != openClawBasePort+1 {
		t.Errorf("second port = %d, want %d", port2, openClawBasePort+1)
	}
	if port3 != openClawBasePort+2 {
		t.Errorf("third port = %d, want %d", port3, openClawBasePort+2)
	}
}

func TestNextGatewayPort_Concurrent(t *testing.T) {
	resetGatewayPort()
	defer resetGatewayPort()

	const goroutines = 50
	ports := make([]int, goroutines)
	errs := make([]error, goroutines)
	var wg sync.WaitGroup

	wg.Add(goroutines)
	for i := range goroutines {
		go func(idx int) {
			defer wg.Done()
			ports[idx], errs[idx] = nextGatewayPort()
		}(i)
	}
	wg.Wait()

	// No errors should have occurred.
	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: nextGatewayPort() returned unexpected error: %v", i, err)
		}
	}

	// All ports must be unique.
	seen := make(map[int]bool, goroutines)
	for _, p := range ports {
		if seen[p] {
			t.Errorf("duplicate port %d allocated", p)
		}
		seen[p] = true
	}

	// All ports must be in the expected range.
	for _, p := range ports {
		if p < openClawBasePort || p >= openClawBasePort+goroutines {
			t.Errorf("port %d out of expected range [%d, %d)", p, openClawBasePort, openClawBasePort+goroutines)
		}
	}
}

func TestPrepareOpenClawWorkspace(t *testing.T) {
	// This test modifies global state (port allocator) via resetGatewayPort,
	// so it must not run in parallel with other tests that use the allocator.
	resetGatewayPort()
	defer resetGatewayPort()

	clusterRoot := t.TempDir()
	agentID := "test-agent"
	spec := testOpenClawManifest(agentID)

	// Create agent source directory with some optional files.
	agentDir := filepath.Join(clusterRoot, "agents", agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "SOUL.md"), []byte("# Soul"), 0644); err != nil {
		t.Fatalf("WriteFile SOUL.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "IDENTITY.md"), []byte("# Identity"), 0644); err != nil {
		t.Fatalf("WriteFile IDENTITY.md: %v", err)
	}

	// Create skills directory with a skill file.
	skillsDir := filepath.Join(agentDir, "skills")
	if err := os.MkdirAll(skillsDir, 0755); err != nil {
		t.Fatalf("MkdirAll skills: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillsDir, "search.md"), []byte("# Search skill"), 0644); err != nil {
		t.Fatalf("WriteFile search.md: %v", err)
	}

	workspacePath, port, err := prepareOpenClawWorkspace(clusterRoot, agentID, spec)
	if err != nil {
		t.Fatalf("prepareOpenClawWorkspace: %v", err)
	}

	// Verify workspace directory was created.
	expectedPath := filepath.Join(clusterRoot, ".state", "agents", agentID, "workspace")
	if workspacePath != expectedPath {
		t.Errorf("workspacePath = %q, want %q", workspacePath, expectedPath)
	}
	if _, err := os.Stat(workspacePath); err != nil {
		t.Errorf("workspace directory does not exist: %v", err)
	}

	// Verify port was allocated.
	if port < openClawBasePort {
		t.Errorf("port = %d, want >= %d", port, openClawBasePort)
	}

	// Verify SOUL.md was copied.
	data, err := os.ReadFile(filepath.Join(workspacePath, "SOUL.md"))
	if err != nil {
		t.Fatalf("ReadFile SOUL.md: %v", err)
	}
	if string(data) != "# Soul" {
		t.Errorf("SOUL.md content = %q, want %q", string(data), "# Soul")
	}

	// Verify IDENTITY.md was copied.
	data, err = os.ReadFile(filepath.Join(workspacePath, "IDENTITY.md"))
	if err != nil {
		t.Fatalf("ReadFile IDENTITY.md: %v", err)
	}
	if string(data) != "# Identity" {
		t.Errorf("IDENTITY.md content = %q, want %q", string(data), "# Identity")
	}

	// Verify USER.md was NOT copied (it doesn't exist in source).
	if _, err := os.Stat(filepath.Join(workspacePath, "USER.md")); !os.IsNotExist(err) {
		t.Error("USER.md should not exist in workspace (not in source)")
	}

	// Verify skills/ directory was copied.
	skillData, err := os.ReadFile(filepath.Join(workspacePath, "skills", "search.md"))
	if err != nil {
		t.Fatalf("ReadFile skills/search.md: %v", err)
	}
	if string(skillData) != "# Search skill" {
		t.Errorf("skills/search.md content = %q, want %q", string(skillData), "# Search skill")
	}

	// Verify openclaw.json was written.
	configData, err := os.ReadFile(filepath.Join(workspacePath, "openclaw.json"))
	if err != nil {
		t.Fatalf("ReadFile openclaw.json: %v", err)
	}
	var cfg openClawConfig
	if err := json.Unmarshal(configData, &cfg); err != nil {
		t.Fatalf("json.Unmarshal openclaw.json: %v", err)
	}
	if cfg.Model.Provider != "anthropic" {
		t.Errorf("config Model.Provider = %q, want %q", cfg.Model.Provider, "anthropic")
	}
	if cfg.Gateway.Port != port {
		t.Errorf("config Gateway.Port = %d, want %d", cfg.Gateway.Port, port)
	}
}

func TestPrepareOpenClawWorkspace_NoAgentDir(t *testing.T) {
	t.Parallel()

	clusterRoot := t.TempDir()
	spec := testOpenClawManifest("missing-agent")

	// Should succeed even without agent source dir — files are optional.
	workspacePath, _, err := prepareOpenClawWorkspace(clusterRoot, "missing-agent", spec)
	if err != nil {
		t.Fatalf("prepareOpenClawWorkspace: %v", err)
	}
	if _, err := os.Stat(workspacePath); err != nil {
		t.Errorf("workspace directory should exist: %v", err)
	}

	// openclaw.json should still be written.
	if _, err := os.Stat(filepath.Join(workspacePath, "openclaw.json")); err != nil {
		t.Errorf("openclaw.json should exist: %v", err)
	}
}

func TestCopyFile(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	src := filepath.Join(tmpDir, "source.txt")
	dst := filepath.Join(tmpDir, "dest.txt")

	content := "hello, world"
	if err := os.WriteFile(src, []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != content {
		t.Errorf("copied content = %q, want %q", string(data), content)
	}
}

func TestCopyDir(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "src")
	dstDir := filepath.Join(tmpDir, "dst")

	// Create nested structure.
	if err := os.MkdirAll(filepath.Join(srcDir, "sub"), 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "a.txt"), []byte("a"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "sub", "b.txt"), []byte("b"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := copyDir(srcDir, dstDir); err != nil {
		t.Fatalf("copyDir: %v", err)
	}

	// Verify files.
	data, err := os.ReadFile(filepath.Join(dstDir, "a.txt"))
	if err != nil {
		t.Fatalf("ReadFile a.txt: %v", err)
	}
	if string(data) != "a" {
		t.Errorf("a.txt = %q, want %q", string(data), "a")
	}

	data, err = os.ReadFile(filepath.Join(dstDir, "sub", "b.txt"))
	if err != nil {
		t.Fatalf("ReadFile sub/b.txt: %v", err)
	}
	if string(data) != "b" {
		t.Errorf("sub/b.txt = %q, want %q", string(data), "b")
	}
}

// ---------------------------------------------------------------------------
// TEST-PORT: releaseGatewayPort and port recycling
// ---------------------------------------------------------------------------

func TestReleaseGatewayPort_Recycling(t *testing.T) {
	// Modifies global state — must not run in parallel with other port tests.
	resetGatewayPort()
	defer resetGatewayPort()

	// Allocate three sequential ports.
	p1, err := nextGatewayPort()
	if err != nil {
		t.Fatalf("nextGatewayPort() [1]: %v", err)
	}
	p2, err := nextGatewayPort()
	if err != nil {
		t.Fatalf("nextGatewayPort() [2]: %v", err)
	}
	_, err = nextGatewayPort()
	if err != nil {
		t.Fatalf("nextGatewayPort() [3]: %v", err)
	}

	// Release the first two ports.
	releaseGatewayPort(p1)
	releaseGatewayPort(p2)

	// Next allocation should recycle p2 (LIFO — last released is returned first).
	recycled1, err := nextGatewayPort()
	if err != nil {
		t.Fatalf("nextGatewayPort() [recycled1]: %v", err)
	}
	if recycled1 != p2 {
		t.Errorf("first recycled port = %d, want %d (LIFO order)", recycled1, p2)
	}

	// Next allocation should recycle p1.
	recycled2, err := nextGatewayPort()
	if err != nil {
		t.Fatalf("nextGatewayPort() [recycled2]: %v", err)
	}
	if recycled2 != p1 {
		t.Errorf("second recycled port = %d, want %d (LIFO order)", recycled2, p1)
	}

	// Next allocation should resume sequential (the next new port after p3).
	p4, err := nextGatewayPort()
	if err != nil {
		t.Fatalf("nextGatewayPort() [4]: %v", err)
	}
	if p4 != openClawBasePort+3 {
		t.Errorf("post-recycling port = %d, want %d", p4, openClawBasePort+3)
	}
}

func TestReleaseGatewayPort_DuplicateRelease(t *testing.T) {
	// Modifies global state — must not run in parallel with other port tests.
	resetGatewayPort()
	defer resetGatewayPort()

	port, err := nextGatewayPort()
	if err != nil {
		t.Fatalf("nextGatewayPort(): %v", err)
	}

	// Release the same port three times — should not panic and should not
	// add duplicates to the pool.
	releaseGatewayPort(port)
	releaseGatewayPort(port)
	releaseGatewayPort(port)

	// Allocate once — we should get the released port back.
	recycled, err := nextGatewayPort()
	if err != nil {
		t.Fatalf("nextGatewayPort() [recycled]: %v", err)
	}
	if recycled != port {
		t.Errorf("recycled port = %d, want %d", recycled, port)
	}

	// The next allocation should be a NEW sequential port (not the duplicate).
	next, err := nextGatewayPort()
	if err != nil {
		t.Fatalf("nextGatewayPort() [next]: %v", err)
	}
	if next == port {
		t.Errorf("got duplicate port %d after single release should have been consumed", port)
	}
	if next != openClawBasePort+1 {
		t.Errorf("next port = %d, want %d", next, openClawBasePort+1)
	}
}

func TestNextGatewayPort_Exhaustion(t *testing.T) {
	// Modifies global state — must not run in parallel with other port tests.
	resetGatewayPort()
	defer resetGatewayPort()

	// Force the allocator to a state near exhaustion.
	openClawPortAllocator.mu.Lock()
	openClawPortAllocator.next = openClawMaxPort
	openClawPortAllocator.released = nil
	openClawPortAllocator.mu.Unlock()

	// Should succeed for the last available port (65535).
	port, err := nextGatewayPort()
	if err != nil {
		t.Fatalf("nextGatewayPort() at max port: %v", err)
	}
	if port != openClawMaxPort {
		t.Errorf("port = %d, want %d", port, openClawMaxPort)
	}

	// Next allocation should fail — all ports exhausted.
	_, err = nextGatewayPort()
	if err == nil {
		t.Fatal("nextGatewayPort() should return error when ports exhausted")
	}
	if !strings.Contains(err.Error(), "port exhaustion") {
		t.Errorf("error message = %q, want to contain 'port exhaustion'", err.Error())
	}

	// Releasing a port should make allocation succeed again.
	releaseGatewayPort(port)

	recycled, err := nextGatewayPort()
	if err != nil {
		t.Fatalf("nextGatewayPort() after release: %v", err)
	}
	if recycled != port {
		t.Errorf("recycled port = %d, want %d", recycled, port)
	}
}

func TestReleaseGatewayPort_ConcurrentReleaseAndAllocate(t *testing.T) {
	resetGatewayPort()
	defer resetGatewayPort()

	const count = 100

	// Allocate ports.
	ports := make([]int, count)
	for i := range count {
		var err error
		ports[i], err = nextGatewayPort()
		if err != nil {
			t.Fatalf("nextGatewayPort() [%d]: %v", i, err)
		}
	}

	// Release all ports concurrently.
	var wg sync.WaitGroup
	wg.Add(count)
	for i := range count {
		go func(p int) {
			defer wg.Done()
			releaseGatewayPort(p)
		}(ports[i])
	}
	wg.Wait()

	// Reallocate all ports — they should all come from the recycled pool.
	seen := make(map[int]bool, count)
	for range count {
		p, err := nextGatewayPort()
		if err != nil {
			t.Fatalf("nextGatewayPort() during re-allocation: %v", err)
		}
		if seen[p] {
			t.Errorf("duplicate port %d during re-allocation", p)
		}
		seen[p] = true
	}

	// All reallocated ports should be from the original set.
	for _, orig := range ports {
		if !seen[orig] {
			t.Errorf("original port %d was not recycled", orig)
		}
	}
}
