package vm

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/hivehq/hive/internal/config"
	"github.com/hivehq/hive/internal/state"
	"github.com/hivehq/hive/internal/types"
)

// Hypervisor is the interface for VM operations. Implementations include
// FirecrackerHypervisor (real KVM) and MockHypervisor (for testing).
type Hypervisor interface {
	// CreateVM creates a new VM from the given configuration. The VM is not
	// started yet; the Firecracker process is spawned and configured.
	// Returns the process PID and any error.
	CreateVM(cfg VMConfig) (int, error)

	// StartVM boots a previously created VM via its API socket.
	StartVM(socketPath string) error

	// StopVM sends a graceful shutdown signal. It sends SIGTERM then waits
	// up to 5 seconds before sending SIGKILL.
	StopVM(socketPath string, pid int) error

	// DestroyVM forcefully terminates the VM process and cleans up the socket.
	DestroyVM(socketPath string, pid int) error

	// IsRunning checks whether a VM process with the given PID is alive.
	IsRunning(pid int) bool
}

// VMConfig holds the configuration for creating a new Firecracker VM.
type VMConfig struct {
	AgentID        string
	SocketPath     string
	RootfsPath     string
	KernelPath     string
	MemoryMB       int
	VCPUs          int
	CID            uint32
	AgentDrivePath string // path to ext4 disk image for agent files (T1-02)
}

// Manager handles Firecracker VM lifecycle. It coordinates between the
// Hypervisor interface, the state.Store for persistence, and the filesystem
// for rootfs copies and socket files.
type Manager struct {
	clusterRoot string
	store       *state.Store
	logger      *slog.Logger
	hypervisor  Hypervisor
	nextCID     uint32
	mu          sync.Mutex
}

// NewManager creates a new VM manager.
func NewManager(clusterRoot string, store *state.Store, logger *slog.Logger, hyp Hypervisor) *Manager {
	return &Manager{
		clusterRoot: clusterRoot,
		store:       store,
		logger:      logger,
		hypervisor:  hyp,
		nextCID:     3, // CIDs 0, 1, 2 are reserved
	}
}

// allocateCID returns the next available unique CID for virtio-vsock.
func (m *Manager) allocateCID() uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()

	cid := m.nextCID
	m.nextCID++
	return cid
}

// StartAgent provisions and boots a VM for the given agent manifest.
// It performs the full lifecycle: validate not already running, set PENDING,
// copy rootfs, CREATING, create VM, STARTING, start VM, then RUNNING.
func (m *Manager) StartAgent(agent *types.AgentManifest) error {
	agentID := agent.Metadata.ID

	// Check if agent is already running.
	existing := m.store.GetAgent(agentID)
	if existing != nil && (existing.Status == state.AgentStatusRunning ||
		existing.Status == state.AgentStatusStarting ||
		existing.Status == state.AgentStatusCreating) {
		return fmt.Errorf("agent %s is already in state %s", agentID, existing.Status)
	}

	m.logger.Info("starting agent", "agent_id", agentID)

	now := time.Now()
	agentState := &state.AgentState{
		ID:             agentID,
		Team:           agent.Metadata.Team,
		Status:         state.AgentStatusPending,
		LastTransition: now,
	}

	if err := m.store.SetAgent(agentState); err != nil {
		return fmt.Errorf("setting initial agent state: %w", err)
	}

	// Resolve resource values from the manifest.
	memoryMB, vcpus, err := m.resolveResources(agent)
	if err != nil {
		return m.failAgent(agentState, fmt.Errorf("resolving resources: %w", err))
	}

	// Transition to CREATING.
	if err := state.ValidateTransition(agentState.Status, state.AgentStatusCreating); err != nil {
		return m.failAgent(agentState, err)
	}
	agentState.Status = state.AgentStatusCreating
	agentState.LastTransition = time.Now()
	if err := m.store.SetAgent(agentState); err != nil {
		return fmt.Errorf("setting agent state to CREATING: %w", err)
	}

	// Prepare filesystem paths.
	stateDir := filepath.Join(m.clusterRoot, ".state", "agents", agentID)
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return m.failAgent(agentState, fmt.Errorf("creating agent state directory: %w", err))
	}

	socketPath := filepath.Join(stateDir, "firecracker.sock")
	rootfsCopy := filepath.Join(stateDir, "rootfs.ext4")
	kernelPath := filepath.Join(m.clusterRoot, "rootfs", "vmlinux")
	baseRootfs := filepath.Join(m.clusterRoot, "rootfs", "rootfs.ext4")
	agentDir := filepath.Join(m.clusterRoot, "agents", agentID)
	agentDriveImg := filepath.Join(stateDir, "agent-drive.img")

	// Copy rootfs for this VM (Firecracker requires a dedicated rootfs per VM).
	if err := copyFile(baseRootfs, rootfsCopy); err != nil {
		return m.failAgent(agentState, fmt.Errorf("copying rootfs for agent %s: %w", agentID, err))
	}

	// T1-02: Create ext4 disk image from agent directory for Firecracker drive API.
	var agentDrivePath string
	if _, err := os.Stat(agentDir); err == nil {
		if err := createAgentDriveImage(agentDir, agentDriveImg); err != nil {
			os.Remove(rootfsCopy)
			return m.failAgent(agentState, fmt.Errorf("creating agent drive image for %s: %w", agentID, err))
		}
		agentDrivePath = agentDriveImg
	}

	// Allocate a unique CID for virtio-vsock.
	cid := m.allocateCID()

	vmCfg := VMConfig{
		AgentID:        agentID,
		SocketPath:     socketPath,
		RootfsPath:     rootfsCopy,
		KernelPath:     kernelPath,
		MemoryMB:       memoryMB,
		VCPUs:          vcpus,
		CID:            cid,
		AgentDrivePath: agentDrivePath,
	}

	// Create the VM via the hypervisor.
	vmPID, err := m.hypervisor.CreateVM(vmCfg)
	if err != nil {
		os.Remove(rootfsCopy) // T1-05: clean up rootfs copy on failure
		return m.failAgent(agentState, fmt.Errorf("creating VM for agent %s: %w", agentID, err))
	}

	// Transition to STARTING.
	if err := state.ValidateTransition(agentState.Status, state.AgentStatusStarting); err != nil {
		os.Remove(rootfsCopy)
		return m.failAgent(agentState, err)
	}
	agentState.Status = state.AgentStatusStarting
	agentState.VMSocketPath = socketPath
	agentState.RootfsCopyPath = rootfsCopy
	agentState.VMCID = cid
	agentState.VMPID = vmPID
	agentState.LastTransition = time.Now()
	if err := m.store.SetAgent(agentState); err != nil {
		return fmt.Errorf("setting agent state to STARTING: %w", err)
	}

	// Start (boot) the VM.
	if err := m.hypervisor.StartVM(socketPath); err != nil {
		os.Remove(rootfsCopy) // T1-05: clean up rootfs copy on failure
		return m.failAgent(agentState, fmt.Errorf("starting VM for agent %s: %w", agentID, err))
	}

	// Transition to RUNNING.
	if err := state.ValidateTransition(agentState.Status, state.AgentStatusRunning); err != nil {
		return m.failAgent(agentState, err)
	}
	agentState.Status = state.AgentStatusRunning
	agentState.StartedAt = time.Now()
	agentState.LastTransition = agentState.StartedAt
	if err := m.store.SetAgent(agentState); err != nil {
		return fmt.Errorf("setting agent state to RUNNING: %w", err)
	}

	m.logger.Info("agent started successfully",
		"agent_id", agentID,
		"cid", cid,
		"socket", socketPath,
	)

	return nil
}

// StopAgent gracefully stops a running agent VM.
func (m *Manager) StopAgent(agentID string) error {
	agentState := m.store.GetAgent(agentID)
	if agentState == nil {
		return fmt.Errorf("agent %s not found in state", agentID)
	}

	if agentState.Status != state.AgentStatusRunning && agentState.Status != state.AgentStatusStarting {
		return fmt.Errorf("agent %s is in state %s, cannot stop", agentID, agentState.Status)
	}

	m.logger.Info("stopping agent", "agent_id", agentID)

	// Transition to STOPPING.
	if err := state.ValidateTransition(agentState.Status, state.AgentStatusStopping); err != nil {
		return fmt.Errorf("validating transition to STOPPING: %w", err)
	}
	agentState.Status = state.AgentStatusStopping
	agentState.LastTransition = time.Now()
	if err := m.store.SetAgent(agentState); err != nil {
		return fmt.Errorf("setting agent state to STOPPING: %w", err)
	}

	// Stop the VM via the hypervisor.
	if err := m.hypervisor.StopVM(agentState.VMSocketPath, agentState.VMPID); err != nil {
		return m.failAgent(agentState, fmt.Errorf("stopping VM for agent %s: %w", agentID, err))
	}

	// Transition to STOPPED.
	if err := state.ValidateTransition(agentState.Status, state.AgentStatusStopped); err != nil {
		return m.failAgent(agentState, err)
	}
	agentState.Status = state.AgentStatusStopped
	agentState.VMPID = 0
	agentState.LastTransition = time.Now()
	if err := m.store.SetAgent(agentState); err != nil {
		return fmt.Errorf("setting agent state to STOPPED: %w", err)
	}

	m.logger.Info("agent stopped", "agent_id", agentID)
	return nil
}

// DestroyAgent stops the agent VM if running, cleans up all artifacts (rootfs
// copy, socket, state directory), and removes the agent from state.
func (m *Manager) DestroyAgent(agentID string) error {
	agentState := m.store.GetAgent(agentID)
	if agentState == nil {
		return fmt.Errorf("agent %s not found in state", agentID)
	}

	m.logger.Info("destroying agent", "agent_id", agentID)

	// If the VM is still running or starting, forcefully destroy it.
	if agentState.Status == state.AgentStatusRunning ||
		agentState.Status == state.AgentStatusStarting ||
		agentState.Status == state.AgentStatusStopping {
		if err := m.hypervisor.DestroyVM(agentState.VMSocketPath, agentState.VMPID); err != nil {
			m.logger.Warn("error destroying VM process, continuing cleanup",
				"agent_id", agentID,
				"error", err,
			)
		}
	}

	// Clean up rootfs copy.
	if agentState.RootfsCopyPath != "" {
		if err := os.Remove(agentState.RootfsCopyPath); err != nil && !os.IsNotExist(err) {
			m.logger.Warn("error removing rootfs copy",
				"agent_id", agentID,
				"path", agentState.RootfsCopyPath,
				"error", err,
			)
		}
	}

	// Clean up socket file.
	if agentState.VMSocketPath != "" {
		if err := os.Remove(agentState.VMSocketPath); err != nil && !os.IsNotExist(err) {
			m.logger.Warn("error removing socket file",
				"agent_id", agentID,
				"path", agentState.VMSocketPath,
				"error", err,
			)
		}
	}

	// Clean up agent state directory.
	stateDir := filepath.Join(m.clusterRoot, ".state", "agents", agentID)
	if err := os.RemoveAll(stateDir); err != nil {
		m.logger.Warn("error removing agent state directory",
			"agent_id", agentID,
			"path", stateDir,
			"error", err,
		)
	}

	// Remove from state store.
	if err := m.store.RemoveAgent(agentID); err != nil {
		return fmt.Errorf("removing agent %s from state: %w", agentID, err)
	}

	m.logger.Info("agent destroyed", "agent_id", agentID)
	return nil
}

// RestartAgent stops the agent if running and starts it again. The restart
// counter is reset on an explicit restart (as opposed to auto-restart which
// would increment it).
func (m *Manager) RestartAgent(agentID string, agent *types.AgentManifest) error {
	m.logger.Info("restarting agent", "agent_id", agentID)

	agentState := m.store.GetAgent(agentID)
	if agentState != nil && (agentState.Status == state.AgentStatusRunning ||
		agentState.Status == state.AgentStatusStarting) {
		if err := m.StopAgent(agentID); err != nil {
			m.logger.Warn("error stopping agent during restart, continuing",
				"agent_id", agentID,
				"error", err,
			)
			// Force destroy if stop fails.
			if agentState.VMPID != 0 {
				_ = m.hypervisor.DestroyVM(agentState.VMSocketPath, agentState.VMPID)
			}
		}
	}

	// Reset the state for a fresh start.
	if agentState != nil {
		agentState.RestartCount = 0
		agentState.Error = ""
		agentState.Status = state.AgentStatusStopped
		agentState.LastTransition = time.Now()
		if err := m.store.SetAgent(agentState); err != nil {
			return fmt.Errorf("resetting agent state for restart: %w", err)
		}
	}

	return m.StartAgent(agent)
}

// ReconcileOnStartup checks all known agents in state against their actual
// process status. VMs whose processes are no longer running are marked FAILED.
// It also restores nextCID to avoid CID collisions with existing VMs.
// This should be called once at hived startup to recover from crashes.
func (m *Manager) ReconcileOnStartup() error {
	m.logger.Info("reconciling VM state on startup")

	agents := m.store.AllAgents()

	// T1-04: Restore nextCID from existing agent CIDs to prevent reuse.
	m.mu.Lock()
	for _, agent := range agents {
		if agent.VMCID >= m.nextCID {
			m.nextCID = agent.VMCID + 1
		}
	}
	m.mu.Unlock()

	for _, agent := range agents {
		switch agent.Status {
		case state.AgentStatusRunning, state.AgentStatusStarting, state.AgentStatusStopping:
			// Check if the process is still alive.
			if agent.VMPID > 0 && !m.hypervisor.IsRunning(agent.VMPID) {
				m.logger.Warn("agent VM process is dead, marking as FAILED",
					"agent_id", agent.ID,
					"pid", agent.VMPID,
					"previous_status", agent.Status,
				)
				agent.Status = state.AgentStatusFailed
				agent.Error = fmt.Sprintf("VM process (PID %d) not found on startup reconciliation", agent.VMPID)
				agent.VMPID = 0
				agent.LastTransition = time.Now()
				if err := m.store.SetAgent(agent); err != nil {
					m.logger.Error("failed to update agent state during reconciliation",
						"agent_id", agent.ID,
						"error", err,
					)
				}
			} else if agent.VMPID == 0 {
				// No PID recorded but in an active state - mark as failed.
				m.logger.Warn("agent in active state but no PID recorded, marking as FAILED",
					"agent_id", agent.ID,
					"status", agent.Status,
				)
				agent.Status = state.AgentStatusFailed
				agent.Error = "no VM PID recorded for active agent"
				agent.LastTransition = time.Now()
				if err := m.store.SetAgent(agent); err != nil {
					m.logger.Error("failed to update agent state during reconciliation",
						"agent_id", agent.ID,
						"error", err,
					)
				}
			}

		case state.AgentStatusCreating:
			// Agent was mid-creation when we crashed - mark as failed.
			m.logger.Warn("agent was in CREATING state, marking as FAILED",
				"agent_id", agent.ID,
			)
			agent.Status = state.AgentStatusFailed
			agent.Error = "interrupted during VM creation"
			agent.LastTransition = time.Now()
			if err := m.store.SetAgent(agent); err != nil {
				m.logger.Error("failed to update agent state during reconciliation",
					"agent_id", agent.ID,
					"error", err,
				)
			}

		case state.AgentStatusPending, state.AgentStatusStopped, state.AgentStatusFailed:
			// These are terminal/idle states, nothing to reconcile.
		}
	}

	m.logger.Info("startup reconciliation complete", "agents_checked", len(agents))
	return nil
}

// resolveResources extracts memory (in MB) and vCPU count from the agent
// manifest, using defaults if not specified.
func (m *Manager) resolveResources(agent *types.AgentManifest) (memoryMB int, vcpus int, err error) {
	memoryMB = 512 // default
	vcpus = 1      // default

	if agent.Spec.Resources.Memory != "" {
		bytes, parseErr := config.ParseMemory(agent.Spec.Resources.Memory)
		if parseErr != nil {
			return 0, 0, fmt.Errorf("parsing memory %q: %w", agent.Spec.Resources.Memory, parseErr)
		}
		memoryMB = int(bytes / (1024 * 1024))
		if memoryMB < 1 {
			memoryMB = 1
		}
	}

	if agent.Spec.Resources.VCPUs > 0 {
		vcpus = agent.Spec.Resources.VCPUs
	}

	return memoryMB, vcpus, nil
}

// failAgent transitions an agent to the FAILED state with an error message.
func (m *Manager) failAgent(agentState *state.AgentState, cause error) error {
	agentState.Status = state.AgentStatusFailed
	agentState.Error = cause.Error()
	agentState.LastTransition = time.Now()
	if err := m.store.SetAgent(agentState); err != nil {
		m.logger.Error("failed to persist FAILED state",
			"agent_id", agentState.ID,
			"original_error", cause,
			"save_error", err,
		)
	}
	return cause
}

// createAgentDriveImage creates an ext4 disk image from an agent's directory
// contents. The image can be attached as a secondary Firecracker drive.
// T1-02: Firecracker requires block device images, not directories.
func createAgentDriveImage(agentDir, imgPath string) error {
	// Calculate the size needed (minimum 4MB to fit ext4 metadata).
	var totalSize int64
	err := filepath.Walk(agentDir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			totalSize += info.Size()
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("calculating agent directory size: %w", err)
	}

	// Add padding for ext4 metadata and overhead (at least 4MB).
	imgSizeMB := (totalSize/(1024*1024) + 4)
	if imgSizeMB < 4 {
		imgSizeMB = 4
	}

	// Create a sparse file of the right size.
	f, err := os.Create(imgPath)
	if err != nil {
		return fmt.Errorf("creating image file: %w", err)
	}
	if err := f.Truncate(imgSizeMB * 1024 * 1024); err != nil {
		f.Close()
		os.Remove(imgPath)
		return fmt.Errorf("sizing image file: %w", err)
	}
	f.Close()

	// Format as ext4.
	mkfs := exec.Command("mkfs.ext4", "-q", "-F", imgPath)
	if out, err := mkfs.CombinedOutput(); err != nil {
		os.Remove(imgPath)
		return fmt.Errorf("mkfs.ext4: %s: %w", string(out), err)
	}

	// Mount, copy files, unmount.
	mountPoint, err := os.MkdirTemp("", "hive-agent-mount-*")
	if err != nil {
		os.Remove(imgPath)
		return fmt.Errorf("creating mount point: %w", err)
	}
	defer os.RemoveAll(mountPoint)

	mount := exec.Command("mount", "-o", "loop", imgPath, mountPoint)
	if out, err := mount.CombinedOutput(); err != nil {
		os.Remove(imgPath)
		return fmt.Errorf("mounting image: %s: %w", string(out), err)
	}

	// Copy agent files into the mounted image.
	cp := exec.Command("cp", "-a", agentDir+"/.", mountPoint+"/")
	if out, err := cp.CombinedOutput(); err != nil {
		exec.Command("umount", mountPoint).Run()
		os.Remove(imgPath)
		return fmt.Errorf("copying agent files: %s: %w", string(out), err)
	}

	umount := exec.Command("umount", mountPoint)
	if out, err := umount.CombinedOutput(); err != nil {
		os.Remove(imgPath)
		return fmt.Errorf("unmounting image: %s: %w", string(out), err)
	}

	return nil
}

// copyFile copies a regular file from src to dst. It creates dst if it does
// not exist, or truncates it if it does.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening source %s: %w", src, err)
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("creating destination %s: %w", dst, err)
	}
	defer func() {
		if cerr := out.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("closing destination %s: %w", dst, cerr)
		}
	}()

	if _, err = io.Copy(out, in); err != nil {
		return fmt.Errorf("copying %s to %s: %w", src, dst, err)
	}

	return nil
}
