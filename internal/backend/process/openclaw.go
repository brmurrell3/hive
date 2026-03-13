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
	"sync"

	"github.com/brmurrell3/hive/internal/types"
)

// openClawBasePort is the first gateway port assigned to OpenClaw agents.
const openClawBasePort = 9300

// openClawPortAllocator provides thread-safe, monotonically increasing port
// numbers for OpenClaw gateway instances. Each agent gets a unique port.
var openClawPortAllocator = struct {
	mu   sync.Mutex
	next int
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
	workspacePath = filepath.Join(clusterRoot, ".state", "agents", agentID, "workspace")
	if err = os.MkdirAll(workspacePath, 0700); err != nil {
		return "", 0, fmt.Errorf("creating openclaw workspace for %q: %w", agentID, err)
	}

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
	port = nextGatewayPort()

	// Generate and write openclaw.json.
	configBytes, err := generateOpenClawConfig(spec, workspacePath, port)
	if err != nil {
		return "", 0, fmt.Errorf("generating openclaw config for %q: %w", agentID, err)
	}
	configPath := filepath.Join(workspacePath, "openclaw.json")
	if err = os.WriteFile(configPath, configBytes, 0600); err != nil {
		return "", 0, fmt.Errorf("writing openclaw.json for %q: %w", agentID, err)
	}

	return workspacePath, port, nil
}

// generateOpenClawConfig produces the JSON bytes for openclaw.json from the
// agent manifest's model configuration.
func generateOpenClawConfig(spec *types.AgentManifest, workspacePath string, port int) ([]byte, error) {
	cfg := openClawConfig{
		Model: openClawModelConfig{
			Provider: spec.Spec.Runtime.Model.Provider,
			Name:     spec.Spec.Runtime.Model.Name,
			Env:      spec.Spec.Runtime.Model.Env,
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
// It is safe for concurrent use.
func nextGatewayPort() int {
	openClawPortAllocator.mu.Lock()
	defer openClawPortAllocator.mu.Unlock()
	port := openClawPortAllocator.next
	openClawPortAllocator.next++
	return port
}

// resetGatewayPort resets the port allocator to the base port.
// This is only intended for use in tests.
func resetGatewayPort() {
	openClawPortAllocator.mu.Lock()
	defer openClawPortAllocator.mu.Unlock()
	openClawPortAllocator.next = openClawBasePort
}

// copyFile copies a single file from src to dst, preserving permissions.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
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
func copyDir(src, dst string) error {
	srcInfo, err := os.Stat(src)
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
