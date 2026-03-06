package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/brmurrell3/hive/internal/config"
	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/types"
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
	AgentDrivePath string // path to ext4 disk image for agent files
}

// MaxSocketPathLen is the maximum length (in bytes) allowed for derived Unix
// socket paths. The POSIX limit is 108 bytes (including null terminator) on
// Linux. We use 104 to leave a small safety margin.
const MaxSocketPathLen = 104

// Manager handles Firecracker VM lifecycle. It coordinates between the
// Hypervisor interface, the state.Store for persistence, and the filesystem
// for rootfs copies and socket files.
type Manager struct {
	clusterRoot            string
	natsPort               uint32 // Port of the local NATS server (for vsock forwarding)
	natsToken              string // Auth token for the NATS server (passed to sidecar via sidecar.conf)
	store                  *state.Store
	logger                 *slog.Logger
	hypervisor             Hypervisor
	nextCID                uint32
	forwarders             map[string]*VsockForwarder // agentID -> VsockForwarder
	skipSocketPathValidation bool // set to true in tests with mock hypervisors
	mu                     sync.Mutex
}

// NewManager creates a new VM manager. natsPort is the port of the local NATS
// server; it is used to set up vsock forwarding so that guest VMs can reach
// NATS via virtio-vsock. Pass 0 to disable vsock forwarding. natsToken is the
// auth token for the embedded NATS server; it is written into each VM's
// sidecar.conf so the sidecar can authenticate when connecting.
func NewManager(clusterRoot string, store *state.Store, logger *slog.Logger, hyp Hypervisor, natsPort uint32, natsToken string) *Manager {
	return &Manager{
		clusterRoot: clusterRoot,
		natsPort:    natsPort,
		natsToken:   natsToken,
		store:       store,
		logger:      logger,
		hypervisor:  hyp,
		nextCID:     3, // CIDs 0, 1, 2 are reserved
		forwarders:  make(map[string]*VsockForwarder),
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

	// Validate Unix socket path length. The longest derived path is
	// socketPath + ".vsock" (used by the vsock forwarder). Unix domain
	// sockets are limited to 108 bytes on Linux (including the null
	// terminator). We use 104 as the limit to provide a small safety margin.
	// Skip validation when using a mock hypervisor (tests) since no real
	// sockets are created and temp dirs on macOS are often very long.
	if !m.skipSocketPathValidation {
		longestPath := socketPath + ".vsock"
		if len(longestPath) > MaxSocketPathLen {
			return m.failAgent(agentState, fmt.Errorf(
				"unix socket path too long (%d bytes, max %d): %s — use a shorter cluster root path",
				len(longestPath), MaxSocketPathLen, longestPath,
			))
		}
	}

	rootfsCopy := filepath.Join(stateDir, "rootfs.ext4")
	kernelPath := filepath.Join(m.clusterRoot, "rootfs", "vmlinux")
	baseRootfs := filepath.Join(m.clusterRoot, "rootfs", "rootfs.ext4")
	agentDir := filepath.Join(m.clusterRoot, "agents", agentID)
	agentDriveImg := filepath.Join(stateDir, "agent-drive.img")

	// Copy rootfs for this VM (Firecracker requires a dedicated rootfs per VM).
	if err := copyFile(baseRootfs, rootfsCopy); err != nil {
		return m.failAgent(agentState, fmt.Errorf("copying rootfs for agent %s: %w", agentID, err))
	}

	// Ensure the agent directory exists so we can write sidecar.conf into it.
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		os.Remove(rootfsCopy)
		return m.failAgent(agentState, fmt.Errorf("creating agent directory for %s: %w", agentID, err))
	}

	// Write sidecar.conf into the agent directory so the VM init script
	// can source it and pass the values to the sidecar binary.
	// VSOCK_PORT must match the port in NATS_URL so the vsock proxy inside
	// the VM listens on the same port that the NATS client connects to.
	natsPort := m.natsPort
	if natsPort == 0 {
		natsPort = 4222
	}
	sidecarConf := filepath.Join(agentDir, "sidecar.conf")
	confContent := fmt.Sprintf("AGENT_ID=%s\nTEAM_ID=%s\nNATS_URL=nats://127.0.0.1:%d\nNATS_TOKEN=%s\nVSOCK_PORT=%d\n",
		agentID, agent.Metadata.Team, natsPort, m.natsToken, natsPort)

	// Pass the runtime command so the sidecar starts the agent workload.
	if agent.Spec.Runtime.Type != "" {
		confContent += fmt.Sprintf("RUNTIME_CMD=%s\n", agent.Spec.Runtime.Type)
	}

	// Pass capabilities as a JSON array so the sidecar can register them.
	if len(agent.Spec.Capabilities) > 0 {
		capsJSON, jsonErr := json.Marshal(agent.Spec.Capabilities)
		if jsonErr != nil {
			os.Remove(rootfsCopy)
			return m.failAgent(agentState, fmt.Errorf("marshaling capabilities for %s: %w", agentID, jsonErr))
		}
		confContent += fmt.Sprintf("CAPABILITIES=%s\n", string(capsJSON))
	}

	if err := os.WriteFile(sidecarConf, []byte(confContent), 0600); err != nil {
		os.Remove(rootfsCopy)
		return m.failAgent(agentState, fmt.Errorf("writing sidecar.conf for %s: %w", agentID, err))
	}

	// Create ext4 disk image from agent directory for Firecracker drive API.
	var agentDrivePath string
	if _, err := os.Stat(agentDir); err == nil {
		if err := createAgentDriveImage(agentDir, agentDriveImg); err != nil {
			os.Remove(rootfsCopy)
			os.Remove(agentDriveImg)
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
		os.Remove(rootfsCopy) // clean up rootfs copy on failure
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

	// Start the vsock forwarder (host side) before booting the VM so the
	// guest can connect to NATS immediately upon boot. The forwarder listens
	// on the Firecracker vsock UDS path with the port suffix and forwards
	// connections to the local NATS server.
	if m.natsPort > 0 {
		vsockUDSPath := socketPath + ".vsock"
		fwd := NewVsockForwarder(
			vsockUDSPath,
			m.natsPort,
			fmt.Sprintf("127.0.0.1:%d", m.natsPort),
			m.logger.With("agent_id", agentID),
		)
		if err := fwd.Start(context.Background()); err != nil {
			m.logger.Warn("failed to start vsock forwarder, VM will lack NATS connectivity",
				"agent_id", agentID,
				"error", err,
			)
			// Non-fatal: the VM can still boot, but the sidecar won't be able
			// to reach NATS. Log and continue.
		} else {
			m.mu.Lock()
			m.forwarders[agentID] = fwd
			m.mu.Unlock()
		}
	}

	// Start (boot) the VM.
	if err := m.hypervisor.StartVM(socketPath); err != nil {
		// Clean up the vsock forwarder if we started one.
		m.stopForwarder(agentID)
		os.Remove(rootfsCopy)    // clean up rootfs copy on failure
		os.Remove(agentDriveImg) // clean up agent drive image on failure
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

	// Stop the vsock forwarder for this agent.
	m.stopForwarder(agentID)

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

	// Stop the vsock forwarder for this agent.
	m.stopForwarder(agentID)

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

	// Restore nextCID from existing agent CIDs to prevent reuse.
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

// stopForwarder stops and removes the VsockForwarder for the given agent, if one exists.
func (m *Manager) stopForwarder(agentID string) {
	m.mu.Lock()
	fwd, ok := m.forwarders[agentID]
	if ok {
		delete(m.forwarders, agentID)
	}
	m.mu.Unlock()

	if ok && fwd != nil {
		fwd.Stop()
		m.logger.Info("vsock forwarder stopped", "agent_id", agentID)
	}
}

// StopAllForwarders stops all active VsockForwarders. Called during Manager shutdown.
func (m *Manager) StopAllForwarders() {
	m.mu.Lock()
	fwds := make(map[string]*VsockForwarder, len(m.forwarders))
	for k, v := range m.forwarders {
		fwds[k] = v
	}
	m.forwarders = make(map[string]*VsockForwarder)
	m.mu.Unlock()

	for agentID, fwd := range fwds {
		fwd.Stop()
		m.logger.Info("vsock forwarder stopped during shutdown", "agent_id", agentID)
	}
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
// Firecracker requires block device images, not directories.
//
// This implementation uses mkfs.ext4 -d to populate the filesystem directly
// from the source directory without requiring mount/umount (which need root or
// CAP_SYS_ADMIN). The -d flag was introduced in e2fsprogs 1.43 (2016).
//
// If mkfs.ext4 is not found in PATH (e.g. on macOS during development), an
// empty placeholder file is created instead. The mock hypervisor used in tests
// never reads the drive image, so this is safe for development and CI on
// non-Linux platforms.
func createAgentDriveImage(agentDir, imgPath string) error {
	mkfsPath, err := exec.LookPath("mkfs.ext4")
	if err != nil {
		// mkfs.ext4 is not available (e.g. macOS). Create an empty placeholder
		// so that the rest of the startup path can proceed. Real deployments on
		// Linux will always have mkfs.ext4 via e2fsprogs.
		slog.Warn("mkfs.ext4 not found in PATH; creating empty agent drive placeholder",
			"agent_dir", agentDir,
			"img_path", imgPath,
		)
		f, createErr := os.Create(imgPath)
		if createErr != nil {
			return fmt.Errorf("creating agent drive placeholder: %w", createErr)
		}
		return f.Close()
	}

	// Calculate the size needed (minimum 4MB to fit ext4 metadata).
	var totalSize int64
	walkErr := filepath.Walk(agentDir, func(_ string, info os.FileInfo, walkEntryErr error) error {
		if walkEntryErr != nil {
			return walkEntryErr
		}
		if !info.IsDir() {
			totalSize += info.Size()
		}
		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("calculating agent directory size: %w", walkErr)
	}

	// Add padding for ext4 metadata and overhead (at least 4MB).
	imgSizeMB := (totalSize/(1024*1024) + 4)
	if imgSizeMB < 4 {
		imgSizeMB = 4
	}

	// mkfs.ext4 -d <source_dir> populates the filesystem from the directory
	// without requiring a loop mount. The size argument is in 1K blocks.
	sizeBlocks := fmt.Sprintf("%dk", imgSizeMB*1024)
	mkfs := exec.Command(mkfsPath, "-q", "-F", "-d", agentDir, imgPath, sizeBlocks)
	if out, runErr := mkfs.CombinedOutput(); runErr != nil {
		os.Remove(imgPath)
		return fmt.Errorf("mkfs.ext4 -d: %s: %w", string(out), runErr)
	}

	return nil
}

// copyFile copies a regular file from src to dst atomically. It writes to a
// temporary file (dst + ".tmp") first, syncs, then renames to dst. This
// prevents a partial rootfs if hived crashes mid-copy.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening source %s: %w", src, err)
	}
	defer in.Close()

	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("creating temp file %s: %w", tmp, err)
	}

	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return fmt.Errorf("copying %s to %s: %w", src, tmp, err)
	}

	// Flush to disk before renaming so the data is durable.
	if err = out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return fmt.Errorf("syncing temp file %s: %w", tmp, err)
	}

	if err = out.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("closing temp file %s: %w", tmp, err)
	}

	// Atomic rename: on the same filesystem this is guaranteed to either
	// fully succeed or leave dst unchanged.
	if err = os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("renaming %s to %s: %w", tmp, dst, err)
	}

	return nil
}
