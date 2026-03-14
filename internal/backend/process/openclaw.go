// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package process

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/brmurrell3/hive/internal/types"
)

// agentIDPattern validates agent IDs to prevent path traversal.
// Must start with an alphanumeric character and contain only alphanumeric,
// dots, underscores, and hyphens.
var agentIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// openClawBasePort is the first gateway port assigned to OpenClaw agents.
const openClawBasePort = 9300

// openClawMaxPort is the highest valid TCP port number.
const openClawMaxPort = 65535

// openClawPortAllocator provides thread-safe port allocation for OpenClaw
// gateway instances. Each agent gets a unique port. Released ports are
// recycled before allocating new ones.
var openClawPortAllocator = struct {
	mu       sync.Mutex
	next     int
	released []int
}{next: openClawBasePort}

// openClawConfig is the JSON configuration written to openclaw.json inside
// the agent workspace. It tells the OpenClaw binary which model to use,
// which port to expose its gateway on, and where the workspace lives.
type openClawConfig struct {
	Model     openClawModelConfig   `json:"model"`
	Gateway   openClawGatewayConfig `json:"gateway"`
	Workspace string                `json:"workspace"`
}

type openClawModelConfig struct {
	Provider string            `json:"provider"`
	Name     string            `json:"name"`
	Env      map[string]string `json:"env,omitempty"`
}

type openClawGatewayConfig struct {
	Port int    `json:"port"`
	Host string `json:"host"`
}

// isOpenClawRuntime returns true when the agent manifest specifies the
// "openclaw" runtime type.
func isOpenClawRuntime(spec *types.AgentManifest) bool {
	return spec.Spec.Runtime.Type == "openclaw"
}

// findOpenClawBinary locates the openclaw binary in PATH. It returns the
// absolute path on success or an error with install instructions.
func findOpenClawBinary() (string, error) {
	path, err := exec.LookPath("openclaw")
	if err != nil {
		return "", fmt.Errorf("openclaw binary not found in PATH: %w\n\n"+
			"Install OpenClaw:\n"+
			"  go install github.com/openclaw/openclaw@latest\n\n"+
			"Or download from https://github.com/openclaw/openclaw/releases", err)
	}
	return path, nil
}

// prepareOpenClawWorkspace sets up the workspace directory for an OpenClaw
// agent. It creates .state/agents/{id}/workspace/, copies optional agent
// files (SOUL.md, USER.md, IDENTITY.md, skills/) from the agent source
// directory, and writes the generated openclaw.json config.
//
// Parameters:
//   - clusterRoot: absolute path to the cluster root directory
//   - agentID: the agent's unique identifier
//   - spec: the agent manifest containing model configuration
//
// Returns the absolute workspace path and the gateway port assigned.
func prepareOpenClawWorkspace(clusterRoot, agentID string, spec *types.AgentManifest) (workspacePath string, port int, err error) {
	// BE-C1: Validate agentID to prevent path traversal.
	if !agentIDPattern.MatchString(agentID) {
		return "", 0, fmt.Errorf("invalid agent ID %q: must match %s", agentID, agentIDPattern.String())
	}

	parentDir := filepath.Join(clusterRoot, ".state", "agents")
	workspacePath = filepath.Join(parentDir, agentID, "workspace")

	// Verify the resolved workspace path doesn't escape the parent directory.
	resolvedWorkspace, err := filepath.Abs(workspacePath)
	if err != nil {
		return "", 0, fmt.Errorf("resolving workspace path for %q: %w", agentID, err)
	}
	resolvedParent, err := filepath.Abs(parentDir)
	if err != nil {
		return "", 0, fmt.Errorf("resolving parent path for %q: %w", agentID, err)
	}
	rel, err := filepath.Rel(resolvedParent, resolvedWorkspace)
	if err != nil || rel == ".." || len(rel) > 2 && rel[:3] == "../" {
		return "", 0, fmt.Errorf("workspace path for agent %q escapes parent directory", agentID)
	}

	if err = os.MkdirAll(workspacePath, 0700); err != nil {
		return "", 0, fmt.Errorf("creating openclaw workspace for %q: %w", agentID, err)
	}

	// BE-H1/BE-C2: Deferred cleanup of workspace directory if any subsequent
	// step fails. The success flag is set just before the final return so
	// that early error returns trigger cleanup.
	success := false
	defer func() {
		if !success {
			os.RemoveAll(workspacePath) //nolint:errcheck // best-effort cleanup on failure path
		}
	}()

	// Copy optional agent files from the agent source directory.
	agentDir := filepath.Join(clusterRoot, "agents", agentID)
	filesToCopy := []string{"SOUL.md", "USER.md", "IDENTITY.md"}
	for _, name := range filesToCopy {
		src := filepath.Join(agentDir, name)
		if _, statErr := os.Stat(src); statErr != nil {
			continue // File does not exist — skip silently.
		}
		if copyErr := copyFile(src, filepath.Join(workspacePath, name)); copyErr != nil {
			return "", 0, fmt.Errorf("copying %s to workspace for %q: %w", name, agentID, copyErr)
		}
	}

	// Copy the skills/ directory if it exists.
	skillsSrc := filepath.Join(agentDir, "skills")
	if info, statErr := os.Stat(skillsSrc); statErr == nil && info.IsDir() {
		if copyErr := copyDir(skillsSrc, filepath.Join(workspacePath, "skills")); copyErr != nil {
			return "", 0, fmt.Errorf("copying skills/ to workspace for %q: %w", agentID, copyErr)
		}
	}

	// Allocate a unique gateway port for this agent.
	port, err = nextGatewayPort()
	if err != nil {
		return "", 0, fmt.Errorf("allocating gateway port for %q: %w", agentID, err)
	}

	// Generate and write openclaw.json.
	configBytes, err := generateOpenClawConfig(spec, workspacePath, port)
	if err != nil {
		return "", 0, fmt.Errorf("generating openclaw config for %q: %w", agentID, err)
	}
	configPath := filepath.Join(workspacePath, "openclaw.json")
	if err = os.WriteFile(configPath, configBytes, 0600); err != nil {
		return "", 0, fmt.Errorf("writing openclaw.json for %q: %w", agentID, err)
	}

	success = true
	return workspacePath, port, nil
}

// filterModelEnv returns a copy of env with dangerous keys removed.
// BE-H1: The same denylist used in Create (HIVE_*, LD_*, DYLD_*, PATH,
// HOME, SHELL) is applied to prevent injection via openclaw.json.
func filterModelEnv(env map[string]string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	filtered := make(map[string]string, len(env))
	for k, v := range env {
		upper := strings.ToUpper(k)
		if strings.HasPrefix(upper, "HIVE_") ||
			strings.HasPrefix(upper, "LD_") ||
			strings.HasPrefix(upper, "DYLD_") ||
			upper == "PATH" || upper == "HOME" || upper == "SHELL" {
			continue
		}
		filtered[k] = v
	}
	if len(filtered) == 0 {
		return nil
	}
	return filtered
}

// generateOpenClawConfig produces the JSON bytes for openclaw.json from the
// agent manifest's model configuration.
func generateOpenClawConfig(spec *types.AgentManifest, workspacePath string, port int) ([]byte, error) {
	cfg := openClawConfig{
		Model: openClawModelConfig{
			Provider: spec.Spec.Runtime.Model.Provider,
			Name:     spec.Spec.Runtime.Model.Name,
			Env:      filterModelEnv(spec.Spec.Runtime.Model.Env), // BE-H1: apply denylist
		},
		Gateway: openClawGatewayConfig{
			Port: port,
			Host: "127.0.0.1",
		},
		Workspace: workspacePath,
	}
	return json.MarshalIndent(cfg, "", "    ")
}

// nextGatewayPort returns the next available OpenClaw gateway port.
// It recycles previously released ports before allocating new ones.
// Returns an error if no ports are available (all ports up to 65535 exhausted).
// It is safe for concurrent use.
func nextGatewayPort() (int, error) {
	openClawPortAllocator.mu.Lock()
	defer openClawPortAllocator.mu.Unlock()

	// Recycle a released port if available.
	if n := len(openClawPortAllocator.released); n > 0 {
		port := openClawPortAllocator.released[n-1]
		openClawPortAllocator.released = openClawPortAllocator.released[:n-1]
		return port, nil
	}

	if openClawPortAllocator.next > openClawMaxPort {
		return 0, fmt.Errorf("gateway port exhaustion: all ports in range [%d, %d] are allocated", openClawBasePort, openClawMaxPort)
	}
	port := openClawPortAllocator.next
	openClawPortAllocator.next++
	return port, nil
}

// releaseGatewayPort returns a port to the pool for reuse.
// It is safe for concurrent use.
func releaseGatewayPort(port int) {
	openClawPortAllocator.mu.Lock()
	defer openClawPortAllocator.mu.Unlock()

	// BE-H2: Check if the port is already in the released slice to prevent
	// duplicate releases from corrupting the pool.
	for _, p := range openClawPortAllocator.released {
		if p == port {
			return
		}
	}
	openClawPortAllocator.released = append(openClawPortAllocator.released, port)
}

// resetGatewayPort resets the port allocator to the base port.
// This is only intended for use in tests.
func resetGatewayPort() {
	openClawPortAllocator.mu.Lock()
	defer openClawPortAllocator.mu.Unlock()
	openClawPortAllocator.next = openClawBasePort
	openClawPortAllocator.released = nil
}

// copyFile copies a single file from src to dst, preserving permissions.
// BE-C2: Uses os.Lstat to verify the source is a regular file before opening,
// preventing symlink-following filesystem escapes.
func copyFile(src, dst string) error {
	// Verify source is a regular file (not a symlink or special file).
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing to copy non-regular file %q (mode: %s)", src, info.Mode().Type())
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

// copyDir recursively copies a directory tree from src to dst.
// BE-C2: Symlink entries are skipped to prevent filesystem escape attacks.
func copyDir(src, dst string) error {
	srcInfo, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(dst, srcInfo.Mode()); err != nil {
		return err
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		// Skip symlinks to prevent filesystem escape.
		if entry.Type()&os.ModeSymlink != 0 {
			continue
		}

		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		if entry.IsDir() {
			if err = copyDir(srcPath, dstPath); err != nil {
				return err
			}
		} else {
			if err = copyFile(srcPath, dstPath); err != nil {
				return err
			}
		}
	}
	return nil
}
