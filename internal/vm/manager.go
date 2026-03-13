// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

// Package vm manages Firecracker microVM lifecycle including creation, resource accounting, and cleanup.
package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/brmurrell3/hive/internal/config"
	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/types"
)

// validMountPathRegex matches safe mount paths (absolute, alphanumeric with /_.-) at runtime.
var runtimeValidMountPathRegex = regexp.MustCompile(`^/[a-zA-Z0-9/_.-]+$`)

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

// VMVolume represents a shared volume drive to be attached to the VM.
type VMVolume struct {
	Name     string // logical name of the volume
	HostPath string // path to the ext4 image on the host
	ReadOnly bool   // whether the volume is read-only inside the guest
}

// VMConfig holds the configuration for creating a new Firecracker VM.
type VMConfig struct {
	AgentID        string
	SocketPath     string
	RootfsPath     string
	KernelPath     string
	MemoryMB       int
	VCPUs          int
	DiskMB         int            // disk size in megabytes for the agent drive image
	CID            uint32
	AgentDrivePath string         // path to ext4 disk image for agent files
	NetworkPolicy  *NetworkPolicy // nil means egress: full (default)
	Volumes        []VMVolume     // optional shared volume drives
}

// MaxSocketPathLen is the maximum length (in bytes) allowed for derived Unix
// socket paths. The POSIX limit is 108 bytes (including null terminator) on
// Linux. We use 104 to leave a small safety margin.
const MaxSocketPathLen = 104

// Manager handles Firecracker VM lifecycle. It coordinates between the
// Hypervisor interface, the state.Store for persistence, and the filesystem
// for rootfs copies and socket files.
type Manager struct {
	clusterRoot              string
	natsPort                 uint32 // Port of the local NATS server (for vsock forwarding)
	natsToken                string // Auth token for the NATS server (passed to sidecar via sidecar.conf)
	store                    *state.Store
	logger                   *slog.Logger
	hypervisor               Hypervisor
	nextCID                  uint32
	forwarders               map[string]*VsockForwarder // agentID -> VsockForwarder
	skipSocketPathValidation bool                       // set to true in tests with mock hypervisors
	rootfsOverride           string                     // if set, use this rootfs image instead of {clusterRoot}/rootfs/rootfs.ext4

	// Resource accounting
	totalMemoryMB  int64    // Total memory available for VMs (0 = unlimited)
	totalVCPUs     int64    // Total vCPUs available for VMs (0 = unlimited)
	allocatedMemMB int64    // Currently allocated memory across all VMs
	allocatedVCPUs int64    // Currently allocated vCPUs across all VMs
	maxVMs         int      // Maximum number of concurrent VMs (0 = unlimited)
	freeCIDs       []uint32 // CID reclamation pool

	mu sync.Mutex
}

// NewManager creates a new VM manager. natsPort is the port of the local NATS
// server; it is used to set up vsock forwarding so that guest VMs can reach
// NATS via virtio-vsock. Pass 0 to disable vsock forwarding. natsToken is the
// auth token for the embedded NATS server; it is written into each VM's
// sidecar.conf so the sidecar can authenticate when connecting.
// totalMemoryMB and totalVCPUs set hard resource limits (0 = unlimited).
func NewManager(clusterRoot string, store *state.Store, logger *slog.Logger, hyp Hypervisor, natsPort uint32, natsToken string, totalMemoryMB int64, totalVCPUs int64) *Manager {
	return &Manager{
		clusterRoot:   clusterRoot,
		natsPort:      natsPort,
		natsToken:     natsToken,
		store:         store,
		logger:        logger,
		hypervisor:    hyp,
		nextCID:       3, // CIDs 0, 1, 2 are reserved
		forwarders:    make(map[string]*VsockForwarder),
		totalMemoryMB: totalMemoryMB,
		totalVCPUs:    totalVCPUs,
	}
}

// SetRootfsOverride sets a custom rootfs image path that overrides the default
// {clusterRoot}/rootfs/rootfs.ext4 location. This is used when a rootfs image
// is provided via --rootfs-path or auto-downloaded by the ImageManager.
func (m *Manager) SetRootfsOverride(path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rootfsOverride = path
}

// resolveBaseRootfs returns the path to the base rootfs image. If a rootfs
// override has been set (via SetRootfsOverride), it is used. Otherwise, the
// default location {clusterRoot}/rootfs/rootfs.ext4 is returned.
func (m *Manager) resolveBaseRootfs() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rootfsOverride != "" {
		return m.rootfsOverride
	}
	return filepath.Join(m.clusterRoot, "rootfs", "rootfs.ext4")
}

// allocateCID returns the next available unique CID for virtio-vsock.
// It first checks the freeCIDs reclamation pool before incrementing nextCID.
// Returns 0 and an error if the CID space is exhausted (overflow).
func (m *Manager) allocateCID() (uint32, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.freeCIDs) > 0 {
		cid := m.freeCIDs[len(m.freeCIDs)-1]
		m.freeCIDs = m.freeCIDs[:len(m.freeCIDs)-1]
		return cid, nil
	}

	cid := m.nextCID
	m.nextCID++

	// Guard against uint32 overflow: if nextCID wrapped to 0 or fell into
	// the reserved range (0, 1, 2), the CID space is exhausted.
	if m.nextCID < 3 {
		// Roll back the allocation — we cannot safely use this CID space.
		m.nextCID = cid
		return 0, fmt.Errorf("CID space exhausted: uint32 overflow (next would be %d)", m.nextCID)
	}

	return cid, nil
}

// maxFreeCIDs is the upper bound on the freeCIDs reclamation pool size.
// Beyond this limit, released CIDs are discarded to prevent unbounded growth.
const maxFreeCIDs = 10000

// releaseCID returns a CID to the reclamation pool for future reuse.
// It is idempotent: releasing a CID that is already in the pool is a no-op.
// If the pool exceeds maxFreeCIDs, the CID is discarded to bound memory usage.
func (m *Manager) releaseCID(cid uint32) {
	if cid < 3 {
		return // reserved CIDs
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check for duplicates to prevent the same CID appearing in freeCIDs
	// multiple times (e.g. if StopAgent and DestroyAgent both release it).
	for _, existing := range m.freeCIDs {
		if existing == cid {
			return
		}
	}

	// Bound the pool size to prevent unbounded memory growth.
	if len(m.freeCIDs) >= maxFreeCIDs {
		return
	}

	m.freeCIDs = append(m.freeCIDs, cid)
}

// checkAndAllocateResources atomically verifies that sufficient resources are
// available to start a VM with the given memory and vCPU requirements, and if
// so, marks them as allocated in a single critical section. Must be called
// with m.mu held.
func (m *Manager) checkAndAllocateResources(memMB int64, vcpus int64) error {
	if m.maxVMs > 0 {
		activeCount := 0
		for _, a := range m.store.AllAgents() {
			if a.Status == state.AgentStatusRunning || a.Status == state.AgentStatusStarting ||
				a.Status == state.AgentStatusCreating {
				activeCount++
			}
		}
		if activeCount >= m.maxVMs {
			return fmt.Errorf("maximum VM count (%d) reached", m.maxVMs)
		}
	}
	if m.totalMemoryMB > 0 && m.allocatedMemMB+memMB > m.totalMemoryMB {
		return fmt.Errorf("insufficient memory: need %dMB, available %dMB", memMB, m.totalMemoryMB-m.allocatedMemMB)
	}
	if m.totalVCPUs > 0 && m.allocatedVCPUs+vcpus > m.totalVCPUs {
		return fmt.Errorf("insufficient vCPUs: need %d, available %d", vcpus, m.totalVCPUs-m.allocatedVCPUs)
	}

	// Resources available — allocate them while still under the lock.
	m.allocatedMemMB += memMB
	m.allocatedVCPUs += vcpus
	return nil
}

// releaseResources returns allocated resources to the pool.
func (m *Manager) releaseResources(memMB int64, vcpus int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.allocatedMemMB -= memMB
	if m.allocatedMemMB < 0 {
		m.logger.Warn("allocated memory went negative, clamping to zero (possible double-release bug)",
			"computed_value", m.allocatedMemMB,
			"released_mb", memMB,
		)
		m.allocatedMemMB = 0
	}
	m.allocatedVCPUs -= vcpus
	if m.allocatedVCPUs < 0 {
		m.logger.Warn("allocated vCPUs went negative, clamping to zero (possible double-release bug)",
			"computed_value", m.allocatedVCPUs,
			"released_vcpus", vcpus,
		)
		m.allocatedVCPUs = 0
	}
}

// StartAgent provisions and boots a VM for the given agent manifest.
// It performs the full lifecycle: validate not already running, set PENDING,
// copy rootfs, CREATING, create VM, STARTING, start VM, then RUNNING.
func (m *Manager) StartAgent(ctx context.Context, agent *types.AgentManifest) error {
	agentID := agent.Metadata.ID

	if err := types.ValidateSubjectComponent("agent_id", agentID); err != nil {
		return fmt.Errorf("invalid agent ID: %w", err)
	}

	// Resolve resource values from the manifest (no lock needed, pure computation).
	memoryMB, vcpus, diskMB, err := m.resolveResources(agent)
	if err != nil {
		return fmt.Errorf("resolving resources for agent %s: %w", agentID, err)
	}

	// Hold the lock across the status check AND resource allocation to
	// prevent a TOCTOU race where two concurrent StartAgent calls both
	// pass the status check and double-allocate resources.
	m.mu.Lock()
	existing := m.store.GetAgent(agentID)
	if existing != nil && (existing.Status == state.AgentStatusRunning ||
		existing.Status == state.AgentStatusStarting ||
		existing.Status == state.AgentStatusCreating) {
		m.mu.Unlock()
		return fmt.Errorf("agent %s is already in state %s", agentID, existing.Status)
	}

	// Check resource availability and allocate atomically.
	if err := m.checkAndAllocateResources(int64(memoryMB), int64(vcpus)); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("resource check for agent %s: %w", agentID, err)
	}
	m.mu.Unlock()

	// Resources are now reserved. If anything below fails, we must release them.
	// The success path sets committed=true to prevent the deferred release.
	committed := false
	cidReleased := true // set to false after CID is allocated
	stateDirCreated := false
	var stateDir string
	var cid uint32
	defer func() {
		if !committed {
			m.releaseResources(int64(memoryMB), int64(vcpus))
		}
		if !cidReleased {
			m.releaseCID(cid)
		}
		if !committed && stateDirCreated && stateDir != "" {
			os.RemoveAll(stateDir)
		}
	}()

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

	if err := state.CheckTransition(agentState.Status, state.AgentStatusCreating); err != nil {
		return m.failAgent(agentState, err)
	}
	agentState.Status = state.AgentStatusCreating
	agentState.LastTransition = time.Now()
	if err := m.store.SetAgent(agentState); err != nil {
		return fmt.Errorf("setting agent state to CREATING: %w", err)
	}

	// Prepare filesystem paths.
	stateDir = filepath.Join(m.clusterRoot, ".state", "agents", agentID)
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return m.failAgent(agentState, fmt.Errorf("creating agent state directory: %w", err))
	}
	stateDirCreated = true

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
	baseRootfs := m.resolveBaseRootfs()
	agentDir := filepath.Join(m.clusterRoot, "agents", agentID)
	agentDriveImg := filepath.Join(stateDir, "agent-drive.img")

	// Copy rootfs for this VM (Firecracker requires a dedicated rootfs per VM).
	if err := copyFile(baseRootfs, rootfsCopy); err != nil {
		return m.failAgent(agentState, fmt.Errorf("copying rootfs for agent %s: %w", agentID, err))
	}

	// Ensure the agent directory exists so we can write sidecar.conf into it.
	// Use 0700 to prevent other users from reading the NATS token in sidecar.conf.
	if err := os.MkdirAll(agentDir, 0700); err != nil {
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

	// Validate NATS token does not contain newline characters that could
	// inject additional config lines into sidecar.conf.
	if strings.ContainsAny(m.natsToken, "\n\r") {
		return fmt.Errorf("NATS token contains newline characters")
	}

	confContent := fmt.Sprintf("AGENT_ID=%s\nTEAM_ID=%s\nNATS_URL=nats://127.0.0.1:%d\nNATS_TOKEN=%s\nVSOCK_PORT=%d\n",
		agentID, agent.Metadata.Team, natsPort, m.natsToken, natsPort)

	// Pass the runtime command so the sidecar starts the agent workload.
	if agent.Spec.Runtime.Type != "" {
		rtType := agent.Spec.Runtime.Type
		if strings.ContainsAny(rtType, "\n\r'\"\\$`") {
			return fmt.Errorf("invalid runtime type contains special characters: %s", rtType)
		}
		confContent += fmt.Sprintf("RUNTIME_CMD=%s\n", rtType)
	}

	// Pass network egress mode so the VM init script can enforce network policy.
	if agent.Spec.Network.Egress != "" {
		egressMode := agent.Spec.Network.Egress
		if strings.ContainsAny(egressMode, "\n\r'\"\\$`") {
			return fmt.Errorf("invalid egress mode contains special characters: %s", egressMode)
		}
		confContent += fmt.Sprintf("HIVE_EGRESS_MODE=%s\n", egressMode)

		// Pass allowlist as JSON array when egress is restricted.
		if egressMode == "restricted" && len(agent.Spec.Network.EgressAllowlist) > 0 {
			allowlistJSON, jsonErr := json.Marshal(agent.Spec.Network.EgressAllowlist)
			if jsonErr != nil {
				os.Remove(rootfsCopy)
				return m.failAgent(agentState, fmt.Errorf("marshaling egress allowlist for %s: %w", agentID, jsonErr))
			}
			if bytes.ContainsAny(allowlistJSON, "\n\r") {
				return fmt.Errorf("egress allowlist JSON contains newline characters")
			}
			confContent += fmt.Sprintf("HIVE_EGRESS_ALLOWLIST='%s'\n", string(allowlistJSON))
		}
	}

	// Pass capabilities as a JSON array so the sidecar can register them.
	if len(agent.Spec.Capabilities) > 0 {
		capsJSON, jsonErr := json.Marshal(agent.Spec.Capabilities)
		if jsonErr != nil {
			os.Remove(rootfsCopy)
			return m.failAgent(agentState, fmt.Errorf("marshaling capabilities for %s: %w", agentID, jsonErr))
		}
		if bytes.ContainsAny(capsJSON, "\n\r") {
			return fmt.Errorf("capabilities JSON contains newline characters")
		}
		confContent += fmt.Sprintf("CAPABILITIES='%s'\n", string(capsJSON))
	}

	// Resolve shared volumes: look up agent volumes against team shared_volumes,
	// create ext4 images for each, and build the HIVE_VOLUMES env var for the
	// guest init script. Shared volume drives appear as /dev/vdc, /dev/vdd, etc.
	// (after vda=rootfs, vdb=agent drive) when an agent drive is present,
	// or /dev/vdb, /dev/vdc, etc. when no agent drive exists.
	var volumeMounts []VMVolume
	var volumeImgPaths []string // track created images for cleanup on failure

	// Determine the guest device letter offset for shared volumes. If an agent
	// drive will be attached (vdb), volumes start at 'c'; otherwise at 'b'.
	hasAgentDrive := false
	if _, statErr := os.Stat(agentDir); statErr == nil {
		hasAgentDrive = true
	}
	volumeDeviceOffset := byte('c')
	if !hasAgentDrive {
		volumeDeviceOffset = 'b'
	}

	if len(agent.Spec.Volumes) > 0 && agent.Metadata.Team != "" {
		teams, teamsErr := config.LoadTeams(m.clusterRoot)
		if teamsErr != nil {
			os.Remove(rootfsCopy)
			return m.failAgent(agentState, fmt.Errorf("loading teams for volume resolution: %w", teamsErr))
		}
		team, teamFound := teams[agent.Metadata.Team]
		if !teamFound {
			os.Remove(rootfsCopy)
			return m.failAgent(agentState, fmt.Errorf("team %q not found for agent %s volume resolution", agent.Metadata.Team, agentID))
		}

		// Build lookup map of team shared volumes.
		svByName := make(map[string]*types.SharedVolume)
		for i := range team.Spec.SharedVolumes {
			svByName[team.Spec.SharedVolumes[i].Name] = &team.Spec.SharedVolumes[i]
		}

		// Guest block devices: vda=rootfs, vdb=agent (if present), then shared volumes.
		// Firecracker supports up to 26 virtio block devices (vda-vdz). With rootfs
		// on vda and optionally agent on vdb, at most 23-24 volumes are possible.
		var hiveVolumeParts []string
		seenMountPaths := make(map[string]bool)
		for i, vol := range agent.Spec.Volumes {
			// Guard against exceeding the virtio block device limit (vda-vdz = 26 devices).
			// vda is rootfs, vdb may be agent drive, leaving at most 23-24 slots.
			if i >= 23 {
				for _, imgPath := range volumeImgPaths {
					os.Remove(imgPath)
				}
				os.Remove(rootfsCopy)
				return m.failAgent(agentState, fmt.Errorf("agent %s has too many volumes (%d): maximum is 23", agentID, len(agent.Spec.Volumes)))
			}

			sv, svFound := svByName[vol.Name]
			if !svFound {
				for _, imgPath := range volumeImgPaths {
					os.Remove(imgPath)
				}
				os.Remove(rootfsCopy)
				return m.failAgent(agentState, fmt.Errorf("agent %s volume %q references nonexistent team shared_volume", agentID, vol.Name))
			}

			readOnly := vol.Access == "ro"
			// If the team shared_volume specifies read-only access, the agent
			// cannot override it to read-write.
			if sv.Access == "ro" || sv.Access == "read-only" {
				readOnly = true
			}

			guestDevice := fmt.Sprintf("/dev/vd%c", volumeDeviceOffset+byte(i))
			accessStr := "rw"
			if readOnly {
				accessStr = "ro"
			}

			// Create an ext4 image from the shared volume's host_path directory.
			volImgPath := filepath.Join(stateDir, fmt.Sprintf("shared-vol-%d.img", i))
			if err := createSharedVolumeImage(ctx, sv.HostPath, volImgPath); err != nil {
				for _, imgPath := range volumeImgPaths {
					os.Remove(imgPath)
				}
				os.Remove(rootfsCopy)
				return m.failAgent(agentState, fmt.Errorf("creating shared volume image for %s vol %q: %w", agentID, vol.Name, err))
			}
			volumeImgPaths = append(volumeImgPaths, volImgPath)

			volumeMounts = append(volumeMounts, VMVolume{
				Name:     vol.Name,
				HostPath: volImgPath, // Firecracker gets the ext4 image path
				ReadOnly: readOnly,
			})

			// Validate mountPath at runtime to prevent format injection in HIVE_VOLUMES.
			if !runtimeValidMountPathRegex.MatchString(vol.MountPath) {
				for _, imgPath := range volumeImgPaths {
					os.Remove(imgPath)
				}
				os.Remove(rootfsCopy)
				return m.failAgent(agentState, fmt.Errorf("agent %s volume %q mountPath %q contains invalid characters", agentID, vol.Name, vol.MountPath))
			}

			// Detect overlapping mount paths.
			if seenMountPaths[vol.MountPath] {
				for _, imgPath := range volumeImgPaths {
					os.Remove(imgPath)
				}
				os.Remove(rootfsCopy)
				return m.failAgent(agentState, fmt.Errorf("agent %s has duplicate volume mountPath %q", agentID, vol.MountPath))
			}
			seenMountPaths[vol.MountPath] = true

			hiveVolumeParts = append(hiveVolumeParts, fmt.Sprintf("%s:%s:%s", guestDevice, vol.MountPath, accessStr))
		}

		if len(hiveVolumeParts) > 0 {
			confContent += fmt.Sprintf("HIVE_VOLUMES=%s\n", strings.Join(hiveVolumeParts, "|"))
		}
	}

	if err := os.WriteFile(sidecarConf, []byte(confContent), 0600); err != nil {
		for _, imgPath := range volumeImgPaths {
			os.Remove(imgPath)
		}
		os.Remove(rootfsCopy)
		return m.failAgent(agentState, fmt.Errorf("writing sidecar.conf for %s: %w", agentID, err))
	}

	// Create ext4 disk image from agent directory for Firecracker drive API.
	var agentDrivePath string
	if _, err := os.Stat(agentDir); err == nil {
		if err := createAgentDriveImage(ctx, agentDir, agentDriveImg, diskMB); err != nil {
			for _, imgPath := range volumeImgPaths {
				os.Remove(imgPath)
			}
			os.Remove(rootfsCopy)
			os.Remove(sidecarConf)
			os.Remove(agentDriveImg)
			return m.failAgent(agentState, fmt.Errorf("creating agent drive image for %s: %w", agentID, err))
		}
		agentDrivePath = agentDriveImg
	}

	// Allocate a unique CID for virtio-vsock.
	var cidErr error
	cid, cidErr = m.allocateCID()
	if cidErr != nil {
		for _, imgPath := range volumeImgPaths {
			os.Remove(imgPath)
		}
		os.Remove(rootfsCopy)
		os.Remove(sidecarConf)
		os.Remove(agentDriveImg)
		return m.failAgent(agentState, fmt.Errorf("allocating CID for agent %s: %w", agentID, cidErr))
	}
	cidReleased = false // CID now allocated; deferred cleanup will release on failure

	// Build network policy for the VM if egress is configured.
	var netPolicy *NetworkPolicy
	if agent.Spec.Network.Egress != "" {
		tapDevice := TapDeviceName(agentID)
		netPolicy = &NetworkPolicy{
			TapDevice: tapDevice,
			Egress:    agent.Spec.Network.Egress,
			Allowlist: agent.Spec.Network.EgressAllowlist,
			Ingress:   agent.Spec.Network.Ingress,
		}
	}

	vmCfg := VMConfig{
		AgentID:        agentID,
		SocketPath:     socketPath,
		RootfsPath:     rootfsCopy,
		KernelPath:     kernelPath,
		MemoryMB:       memoryMB,
		VCPUs:          vcpus,
		DiskMB:         diskMB,
		CID:            cid,
		AgentDrivePath: agentDrivePath,
		NetworkPolicy:  netPolicy,
		Volumes:        volumeMounts,
	}

	vmPID, err := m.hypervisor.CreateVM(vmCfg)
	if err != nil {
		os.Remove(rootfsCopy)
		os.Remove(sidecarConf)
		os.Remove(agentDriveImg)
		return m.failAgent(agentState, fmt.Errorf("creating VM for agent %s: %w", agentID, err))
	}

	if err := state.CheckTransition(agentState.Status, state.AgentStatusStarting); err != nil {
		os.Remove(rootfsCopy)
		os.Remove(sidecarConf)
		os.Remove(agentDriveImg)
		return m.failAgent(agentState, err)
	}
	agentState.Status = state.AgentStatusStarting
	agentState.VMSocketPath = socketPath
	agentState.RootfsCopyPath = rootfsCopy
	agentState.VMCID = cid
	agentState.VMPID = vmPID
	agentState.MemoryBytes = int64(memoryMB) * 1024 * 1024
	agentState.VCPUs = vcpus
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
		if err := fwd.Start(ctx); err != nil {
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

	// Apply network policy BEFORE booting the VM to prevent a race
	// window where the VM runs without network restrictions.
	// Check both egress and ingress: even if egress is "full", ingress
	// restrictions still require nftables rules.
	needsNftables := (agent.Spec.Network.Egress != "" && agent.Spec.Network.Egress != egressFull) ||
		(agent.Spec.Network.Ingress != "" && agent.Spec.Network.Ingress != egressFull)
	if needsNftables {
		tapDevice := TapDeviceName(agentID)
		policy := NetworkPolicy{
			TapDevice: tapDevice,
			Egress:    agent.Spec.Network.Egress,
			Allowlist: agent.Spec.Network.EgressAllowlist,
			Ingress:   agent.Spec.Network.Ingress,
		}
		rules := GenerateNftables(policy)
		if rules != "" {
			nftCtx, nftCancel := context.WithTimeout(ctx, 30*time.Second)
			defer nftCancel()
			nftCmd := exec.CommandContext(nftCtx, "nft", "-f", "-")
			nftCmd.Stdin = strings.NewReader(rules)
			if out, nftErr := nftCmd.CombinedOutput(); nftErr != nil {
				// Fatal: refuse to start the VM without network policy.
				if destroyErr := m.hypervisor.DestroyVM(socketPath, vmPID); destroyErr != nil {
					m.logger.Warn("failed to destroy VM after nftables failure",
						"agent_id", agentID,
						"error", destroyErr,
					)
				}
				m.stopForwarder(agentID)
				os.Remove(rootfsCopy)
				os.Remove(sidecarConf)
				os.Remove(agentDriveImg)
				return m.failAgent(agentState, fmt.Errorf("applying nftables rules for agent %s: %w (output: %s)", agentID, nftErr, string(out)))
			}
			m.logger.Info("nftables rules applied", "agent_id", agentID, "egress", agent.Spec.Network.Egress)
		}
	}

	if err := m.hypervisor.StartVM(socketPath); err != nil {
		// Clean up nftables rules that were applied before StartVM.
		if needsNftables {
			tapDevice := TapDeviceName(agentID)
			nftCmd, nftArgs := CleanupNftables(tapDevice)
			nftCtx2, nftCancel2 := context.WithTimeout(ctx, 30*time.Second)
			defer nftCancel2()
			if out, cleanErr := exec.CommandContext(nftCtx2, nftCmd, nftArgs...).CombinedOutput(); cleanErr != nil {
				m.logger.Debug("nftables cleanup after StartVM failure", "agent_id", agentID, "error", cleanErr, "output", string(out))
			}
		}
		if destroyErr := m.hypervisor.DestroyVM(socketPath, vmPID); destroyErr != nil {
			m.logger.Warn("failed to destroy VM after StartVM failure",
				"agent_id", agentID,
				"error", destroyErr,
			)
		}
		m.stopForwarder(agentID)
		os.Remove(rootfsCopy)
		os.Remove(sidecarConf)
		os.Remove(agentDriveImg)
		return m.failAgent(agentState, fmt.Errorf("starting VM for agent %s: %w", agentID, err))
	}

	// VM is now running. Mark resources as committed so the deferred cleanup
	// does not release CID/resources that belong to the running VM. If any
	// subsequent step fails, we perform explicit cleanup of the running VM.
	committed = true
	cidReleased = true

	if err := state.CheckTransition(agentState.Status, state.AgentStatusRunning); err != nil {
		// VM is running but state transition failed. Explicitly destroy the VM
		// and release resources since the deferred function considers them committed.
		_ = m.hypervisor.DestroyVM(socketPath, vmPID)
		m.stopForwarder(agentID)
		m.releaseResources(int64(memoryMB), int64(vcpus))
		m.releaseCID(cid)
		return m.failAgent(agentState, err)
	}
	agentState.Status = state.AgentStatusRunning
	agentState.StartedAt = time.Now()
	agentState.LastTransition = agentState.StartedAt
	if err := m.store.SetAgent(agentState); err != nil {
		// VM is running but state persistence failed. Explicitly destroy the VM
		// and release resources since the deferred function considers them committed.
		_ = m.hypervisor.DestroyVM(socketPath, vmPID)
		m.stopForwarder(agentID)
		m.releaseResources(int64(memoryMB), int64(vcpus))
		m.releaseCID(cid)
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
// Note: StopAgent intentionally does not clean up the agent drive image or
// rootfs copy. This is by design so that the agent can be restarted without
// re-creating these artifacts. DestroyAgent handles full artifact cleanup.
func (m *Manager) StopAgent(ctx context.Context, agentID string) error {
	if err := types.ValidateSubjectComponent("agent_id", agentID); err != nil {
		return fmt.Errorf("invalid agent ID: %w", err)
	}

	agentState := m.store.GetAgent(agentID)
	if agentState == nil {
		return fmt.Errorf("agent %s not found in state", agentID)
	}

	if agentState.Status != state.AgentStatusRunning && agentState.Status != state.AgentStatusStarting {
		return fmt.Errorf("agent %s is in state %s, cannot stop", agentID, agentState.Status)
	}

	m.logger.Info("stopping agent", "agent_id", agentID)

	// Atomically transition to STOPPING using ModifyAgent to prevent
	// TOCTOU races between the status check above and the state write.
	if err := m.store.ModifyAgent(agentID, func(a *state.AgentState) error {
		if a.Status != state.AgentStatusRunning && a.Status != state.AgentStatusStarting {
			return fmt.Errorf("agent %s is in state %s, cannot stop", agentID, a.Status)
		}
		a.Status = state.AgentStatusStopping
		a.LastTransition = time.Now()
		return nil
	}); err != nil {
		return fmt.Errorf("transitioning agent to STOPPING: %w", err)
	}

	// Re-read the agent state after the atomic transition so subsequent
	// code operates on the latest snapshot.
	agentState = m.store.GetAgent(agentID)
	if agentState == nil {
		return fmt.Errorf("agent %s disappeared after STOPPING transition", agentID)
	}

	m.stopForwarder(agentID)

	if err := m.hypervisor.StopVM(agentState.VMSocketPath, agentState.VMPID); err != nil {
		return m.failAgent(agentState, fmt.Errorf("stopping VM for agent %s: %w", agentID, err))
	}

	// Clean up nftables rules for this agent's tap device.
	tapDevice := TapDeviceName(agentID)
	nftCmd, nftArgs := CleanupNftables(tapDevice)
	nftCtx, nftCancel := context.WithTimeout(ctx, 30*time.Second)
	defer nftCancel()
	if out, err := exec.CommandContext(nftCtx, nftCmd, nftArgs...).CombinedOutput(); err != nil {
		m.logger.Debug("nftables cleanup skipped", "agent_id", agentID, "error", err, "output", string(out))
	}

	// Atomically capture and zero resource fields inside ModifyAgent to
	// prevent TOCTOU double-release (C2): a concurrent DestroyAgent could
	// zero the fields between our snapshot read and release call.
	var releaseMem int64
	var releaseVCPUs int64
	var releaseCID uint32
	if err := m.store.ModifyAgent(agentID, func(a *state.AgentState) error {
		releaseMem = a.MemoryBytes / (1024 * 1024)
		releaseVCPUs = int64(a.VCPUs)
		releaseCID = a.VMCID
		a.Status = state.AgentStatusStopped
		a.VMPID = 0
		a.MemoryBytes = 0
		a.VCPUs = 0
		a.VMCID = 0
		a.Error = ""
		a.LastTransition = time.Now()
		return nil
	}); err != nil {
		return fmt.Errorf("setting agent state to STOPPED: %w", err)
	}

	// Release resources and CID after ModifyAgent returns to avoid lock
	// inversion (m.mu must not be acquired while store.mu is held).
	if releaseMem > 0 || releaseVCPUs > 0 {
		m.releaseResources(releaseMem, releaseVCPUs)
	}
	if releaseCID >= 3 {
		m.releaseCID(releaseCID)
	}

	m.logger.Info("agent stopped", "agent_id", agentID)
	return nil
}

// DestroyAgent stops the agent VM if running, cleans up all artifacts (rootfs
// copy, socket, state directory), and removes the agent from state.
func (m *Manager) DestroyAgent(ctx context.Context, agentID string) error {
	if err := types.ValidateSubjectComponent("agent_id", agentID); err != nil {
		return fmt.Errorf("invalid agent ID: %w", err)
	}

	agentState := m.store.GetAgent(agentID)
	if agentState == nil {
		return fmt.Errorf("agent %s not found in state", agentID)
	}

	m.logger.Info("destroying agent", "agent_id", agentID)

	m.stopForwarder(agentID)

	// Atomically capture VM process info and zero resource fields to prevent
	// double-release if DestroyAgent races with StopAgent. We capture
	// VMSocketPath and VMPID inside the callback to avoid using stale values
	// from the initial GetAgent snapshot (a concurrent StopAgent could zero them).
	var capturedSocketPath string
	var capturedRootfsCopyPath string
	var capturedPID int
	var capturedStatus string
	var releaseMem int64
	var releaseVCPUs int64
	var releaseCID uint32
	if err := m.store.ModifyAgent(agentID, func(a *state.AgentState) error {
		capturedSocketPath = a.VMSocketPath
		capturedRootfsCopyPath = a.RootfsCopyPath
		capturedPID = a.VMPID
		capturedStatus = string(a.Status)
		releaseMem = a.MemoryBytes / (1024 * 1024)
		releaseVCPUs = int64(a.VCPUs)
		releaseCID = a.VMCID
		// Zero resource fields inside the callback so a concurrent StopAgent
		// sees zeroed values and does not double-release. The actual
		// releaseResources/releaseCID calls happen AFTER ModifyAgent returns
		// to avoid lock inversion (m.mu must not be acquired while store.mu
		// is held by ModifyAgent).
		a.MemoryBytes = 0
		a.VCPUs = 0
		a.VMCID = 0
		return nil
	}); err != nil {
		m.logger.Warn("failed to atomically read/zero agent resources during destroy",
			"agent_id", agentID,
			"error", err,
		)
		// Fall back to the snapshot values if ModifyAgent fails (agent may
		// already be removed from state). Use the initial snapshot for cleanup.
		capturedSocketPath = agentState.VMSocketPath
		capturedRootfsCopyPath = agentState.RootfsCopyPath
		capturedPID = agentState.VMPID
		capturedStatus = string(agentState.Status)
	}

	// Release resources and CID outside the ModifyAgent callback to avoid
	// lock inversion (C1): releaseResources/releaseCID acquire m.mu, which
	// must not be held while store.mu is held by ModifyAgent.
	if releaseMem > 0 || releaseVCPUs > 0 {
		m.releaseResources(releaseMem, releaseVCPUs)
	}
	if releaseCID >= 3 {
		m.releaseCID(releaseCID)
	}

	// If the VM is still running or starting, forcefully destroy it.
	if capturedStatus == string(state.AgentStatusRunning) ||
		capturedStatus == string(state.AgentStatusStarting) ||
		capturedStatus == string(state.AgentStatusStopping) {
		if err := m.hypervisor.DestroyVM(capturedSocketPath, capturedPID); err != nil {
			m.logger.Warn("error destroying VM process, continuing cleanup",
				"agent_id", agentID,
				"error", err,
			)
		}
	}

	// Clean up nftables rules for this agent's tap device.
	tapDevice := TapDeviceName(agentID)
	nftCmd, nftArgs := CleanupNftables(tapDevice)
	{
		nftCtx, nftCancel := context.WithTimeout(ctx, 30*time.Second)
		defer nftCancel()
		if out, err := exec.CommandContext(nftCtx, nftCmd, nftArgs...).CombinedOutput(); err != nil {
			m.logger.Debug("nftables cleanup skipped during destroy", "agent_id", agentID, "error", err, "output", string(out))
		}
	}

	if capturedRootfsCopyPath != "" {
		if err := os.Remove(capturedRootfsCopyPath); err != nil && !os.IsNotExist(err) {
			m.logger.Warn("error removing rootfs copy",
				"agent_id", agentID,
				"path", capturedRootfsCopyPath,
				"error", err,
			)
		}
	}

	if capturedSocketPath != "" {
		if err := os.Remove(capturedSocketPath); err != nil && !os.IsNotExist(err) {
			m.logger.Warn("error removing socket file",
				"agent_id", agentID,
				"path", capturedSocketPath,
				"error", err,
			)
		}
	}

	stateDir := filepath.Join(m.clusterRoot, ".state", "agents", agentID)
	if err := os.RemoveAll(stateDir); err != nil {
		m.logger.Warn("error removing agent state directory",
			"agent_id", agentID,
			"path", stateDir,
			"error", err,
		)
	}

	if err := m.store.RemoveAgent(agentID); err != nil {
		return fmt.Errorf("removing agent %s from state: %w", agentID, err)
	}

	m.logger.Info("agent destroyed", "agent_id", agentID)
	return nil
}

// RestartAgent stops the agent if running and starts it again. The restart
// counter is reset on an explicit restart (as opposed to auto-restart which
// would increment it).
func (m *Manager) RestartAgent(ctx context.Context, agentID string, agent *types.AgentManifest) error {
	if err := types.ValidateSubjectComponent("agent_id", agentID); err != nil {
		return fmt.Errorf("invalid agent ID: %w", err)
	}

	m.logger.Info("restarting agent", "agent_id", agentID)

	agentState := m.store.GetAgent(agentID)
	if agentState != nil && (agentState.Status == state.AgentStatusRunning ||
		agentState.Status == state.AgentStatusStarting) {
		if err := m.StopAgent(ctx, agentID); err != nil {
			m.logger.Warn("error stopping agent during restart, continuing",
				"agent_id", agentID,
				"error", err,
			)
			// Re-read agent state from the store before using VMPID/VMSocketPath
			// for the force-destroy fallback. StopAgent may have partially updated
			// the state, so the original agentState snapshot could be stale.
			current := m.store.GetAgent(agentID)
			if current != nil && current.VMPID != 0 {
				_ = m.hypervisor.DestroyVM(current.VMSocketPath, current.VMPID)
			} else if current == nil && agentState.VMPID != 0 {
				// Agent was removed from store; use the original snapshot as fallback.
				_ = m.hypervisor.DestroyVM(agentState.VMSocketPath, agentState.VMPID)
			}
			// Release resources that StopAgent failed to release. Atomically
			// capture and zero the resource fields to prevent double-release
			// if a concurrent operation also attempts cleanup.
			var releaseMem int64
			var releaseVCPUs int64
			var releaseCID uint32
			if modErr := m.store.ModifyAgent(agentID, func(a *state.AgentState) error {
				releaseMem = a.MemoryBytes / (1024 * 1024)
				releaseVCPUs = int64(a.VCPUs)
				releaseCID = a.VMCID
				a.MemoryBytes = 0
				a.VCPUs = 0
				a.VMCID = 0
				return nil
			}); modErr != nil {
				m.logger.Warn("failed to zero agent resources during restart fallback",
					"agent_id", agentID, "error", modErr)
			} else {
				if releaseMem > 0 || releaseVCPUs > 0 {
					m.releaseResources(releaseMem, releaseVCPUs)
				}
				if releaseCID >= 3 {
					m.releaseCID(releaseCID)
				}
			}
		}
	}

	// Reset the state for a fresh start using ModifyAgent for atomicity.
	if agentState != nil {
		if err := m.store.ModifyAgent(agentID, func(a *state.AgentState) error {
			a.RestartCount = 0
			a.Error = ""
			a.Status = state.AgentStatusStopped
			a.LastTransition = time.Now()
			return nil
		}); err != nil {
			m.logger.Warn("failed to reset agent state for restart", "agent", agentID, "error", err)
		}
	}

	return m.StartAgent(ctx, agent)
}

// ReconcileOnStartup checks all known agents in state against their actual
// process status. VMs whose processes are no longer running are marked FAILED.
// It also restores nextCID to avoid CID collisions with existing VMs.
// This should be called once at hived startup to recover from crashes.
func (m *Manager) ReconcileOnStartup() error {
	m.logger.Info("reconciling VM state on startup")

	agents := m.store.AllAgents()

	// Restore nextCID and resource allocations from existing agent state
	// to prevent CID reuse and ensure accurate resource accounting.
	// Zero out allocations first to prevent double-counting on repeated invocations.
	m.mu.Lock()
	m.allocatedMemMB = 0
	m.allocatedVCPUs = 0
	for _, agent := range agents {
		if agent.VMCID >= m.nextCID {
			m.nextCID = agent.VMCID + 1
		}
		// Rebuild resource allocations from agents that are actively consuming resources.
		if agent.Status == state.AgentStatusRunning || agent.Status == state.AgentStatusPending ||
			agent.Status == state.AgentStatusStarting || agent.Status == state.AgentStatusCreating {
			m.allocatedMemMB += agent.MemoryBytes / (1024 * 1024)
			m.allocatedVCPUs += int64(agent.VCPUs)
		}
	}
	m.mu.Unlock()

	for _, agent := range agents {
		if err := types.ValidateSubjectComponent("agent_id", agent.ID); err != nil {
			m.logger.Error("invalid agent ID in store, skipping", "agent_id", agent.ID, "error", err)
			continue
		}

		switch agent.Status {
		case state.AgentStatusRunning, state.AgentStatusStarting, state.AgentStatusStopping:
			// Check if the process is still alive.
			if agent.VMPID > 0 && !m.hypervisor.IsRunning(agent.VMPID) {
				m.logger.Warn("agent VM process is dead, marking as FAILED",
					"agent_id", agent.ID,
					"pid", agent.VMPID,
					"previous_status", agent.Status,
				)

				// Clean up nftables rules for the dead agent's tap device.
				tapDevice := TapDeviceName(agent.ID)
				nftCmd, nftArgs := CleanupNftables(tapDevice)
				nftCtx, nftCancel := context.WithTimeout(context.Background(), 30*time.Second)
				if out, err := exec.CommandContext(nftCtx, nftCmd, nftArgs...).CombinedOutput(); err != nil {
					m.logger.Warn("nftables cleanup failed during reconciliation", "agent_id", agent.ID, "error", err, "output", string(out))
				}
				nftCancel()

				// Clean up disk artifacts left behind by the dead VM.
				m.cleanupAgentArtifacts(agent.ID)

				// Atomically transition to FAILED and capture resource values for release.
				agentMemMB := agent.MemoryBytes / (1024 * 1024)
				agentVCPUs := int64(agent.VCPUs)
				agentCID := agent.VMCID
				failErr := fmt.Sprintf("VM process (PID %d) not found on startup reconciliation", agent.VMPID)

				if err := m.store.ModifyAgent(agent.ID, func(a *state.AgentState) error {
					a.Status = state.AgentStatusFailed
					a.Error = failErr
					a.VMPID = 0
					a.MemoryBytes = 0
					a.VCPUs = 0
					a.VMCID = 0
					a.LastTransition = time.Now()
					return nil
				}); err != nil {
					m.logger.Error("failed to update agent state during reconciliation",
						"agent_id", agent.ID,
						"error", err,
					)
				}

				// Release resources and CID after persisting the FAILED state.
				if agentMemMB > 0 || agentVCPUs > 0 {
					m.releaseResources(agentMemMB, agentVCPUs)
				}
				if agentCID >= 3 {
					m.releaseCID(agentCID)
				}
			} else if agent.VMPID == 0 {
				// No PID recorded but in an active state - mark as failed.
				m.logger.Warn("agent in active state but no PID recorded, marking as FAILED",
					"agent_id", agent.ID,
					"status", agent.Status,
				)

				// Clean up nftables rules for the orphaned agent.
				tapDevice := TapDeviceName(agent.ID)
				nftCmd, nftArgs := CleanupNftables(tapDevice)
				nftCtx, nftCancel := context.WithTimeout(context.Background(), 30*time.Second)
				if out, err := exec.CommandContext(nftCtx, nftCmd, nftArgs...).CombinedOutput(); err != nil {
					m.logger.Warn("nftables cleanup failed during reconciliation", "agent_id", agent.ID, "error", err, "output", string(out))
				}
				nftCancel()

				// Clean up disk artifacts.
				m.cleanupAgentArtifacts(agent.ID)

				// Atomically transition to FAILED and capture resource values for release.
				agentMemMB := agent.MemoryBytes / (1024 * 1024)
				agentVCPUs := int64(agent.VCPUs)
				agentCID := agent.VMCID

				if err := m.store.ModifyAgent(agent.ID, func(a *state.AgentState) error {
					a.Status = state.AgentStatusFailed
					a.Error = "no VM PID recorded for active agent"
					a.MemoryBytes = 0
					a.VCPUs = 0
					a.VMCID = 0
					a.LastTransition = time.Now()
					return nil
				}); err != nil {
					m.logger.Error("failed to update agent state during reconciliation",
						"agent_id", agent.ID,
						"error", err,
					)
				}

				// Release resources and CID after persisting the FAILED state.
				if agentMemMB > 0 || agentVCPUs > 0 {
					m.releaseResources(agentMemMB, agentVCPUs)
				}
				if agentCID >= 3 {
					m.releaseCID(agentCID)
				}
			}

		case state.AgentStatusCreating:
			// Agent was mid-creation when we crashed - mark as failed.
			m.logger.Warn("agent was in CREATING state, marking as FAILED",
				"agent_id", agent.ID,
			)

			// Clean up disk artifacts from interrupted creation.
			m.cleanupAgentArtifacts(agent.ID)

			// Atomically transition to FAILED and capture resource values for release.
			agentMemMB := agent.MemoryBytes / (1024 * 1024)
			agentVCPUs := int64(agent.VCPUs)
			agentCID := agent.VMCID

			if err := m.store.ModifyAgent(agent.ID, func(a *state.AgentState) error {
				a.Status = state.AgentStatusFailed
				a.Error = "interrupted during VM creation"
				a.MemoryBytes = 0
				a.VCPUs = 0
				a.VMCID = 0
				a.LastTransition = time.Now()
				return nil
			}); err != nil {
				m.logger.Error("failed to update agent state during reconciliation",
					"agent_id", agent.ID,
					"error", err,
				)
			}

			// Release resources and CID after persisting the FAILED state.
			if agentMemMB > 0 || agentVCPUs > 0 {
				m.releaseResources(agentMemMB, agentVCPUs)
			}
			if agentCID >= 3 {
				m.releaseCID(agentCID)
			}

		case state.AgentStatusPending:
			// Agent was PENDING at startup (never fully created). Mark as FAILED
			// so the reconciler can re-create it on the next pass.
			m.logger.Warn("agent was PENDING at startup, marking FAILED for recovery", "agent", agent.ID)

			// Clean up any partial disk artifacts.
			m.cleanupAgentArtifacts(agent.ID)

			agentMemMB := agent.MemoryBytes / (1024 * 1024)
			agentVCPUs := int64(agent.VCPUs)
			agentCID := agent.VMCID

			if err := m.store.ModifyAgent(agent.ID, func(a *state.AgentState) error {
				a.Status = state.AgentStatusFailed
				a.Error = "interrupted during PENDING state"
				a.MemoryBytes = 0
				a.VCPUs = 0
				a.VMCID = 0
				a.LastTransition = time.Now()
				return nil
			}); err != nil {
				m.logger.Error("failed to update agent state during reconciliation",
					"agent_id", agent.ID,
					"error", err,
				)
			}

			// Release resources and CID after persisting the FAILED state.
			if agentMemMB > 0 || agentVCPUs > 0 {
				m.releaseResources(agentMemMB, agentVCPUs)
			}
			if agentCID >= 3 {
				m.releaseCID(agentCID)
			}

		case state.AgentStatusStopped, state.AgentStatusFailed:
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

// cleanupAgentArtifacts removes all disk artifacts for a given agent: rootfs copy,
// socket files, agent drive images, and the agent state directory.
func (m *Manager) cleanupAgentArtifacts(agentID string) {
	stateDir := filepath.Join(m.clusterRoot, ".state", "agents", agentID)

	// Remove rootfs copy.
	rootfsCopy := filepath.Join(stateDir, "rootfs.ext4")
	if err := os.Remove(rootfsCopy); err != nil && !os.IsNotExist(err) {
		m.logger.Debug("error removing rootfs copy during cleanup", "agent_id", agentID, "path", rootfsCopy, "error", err)
	}

	// Remove socket file and vsock-related files.
	socketPath := filepath.Join(stateDir, "firecracker.sock")
	os.Remove(socketPath)
	os.Remove(socketPath + ".vsock")
	matches, globErr := filepath.Glob(socketPath + ".vsock_*")
	if globErr != nil {
		m.logger.Warn("filepath.Glob failed during artifact cleanup", "pattern", socketPath+".vsock_*", "error", globErr)
	}
	for _, match := range matches {
		os.Remove(match)
	}

	// Remove agent drive image.
	agentDriveImg := filepath.Join(stateDir, "agent-drive.img")
	if err := os.Remove(agentDriveImg); err != nil && !os.IsNotExist(err) {
		m.logger.Debug("error removing agent drive image during cleanup", "agent_id", agentID, "path", agentDriveImg, "error", err)
	}

	// Remove shared volume images (shared-vol-0.img, shared-vol-1.img, ...).
	volMatches, volGlobErr := filepath.Glob(filepath.Join(stateDir, "shared-vol-*.img"))
	if volGlobErr != nil {
		m.logger.Warn("filepath.Glob failed during shared volume cleanup", "pattern", "shared-vol-*.img", "error", volGlobErr)
	}
	for _, volImg := range volMatches {
		if err := os.Remove(volImg); err != nil && !os.IsNotExist(err) {
			m.logger.Debug("error removing shared volume image during cleanup", "agent_id", agentID, "path", volImg, "error", err)
		}
	}

	// Remove firecracker log file.
	logFile := filepath.Join(stateDir, "firecracker.log")
	if err := os.Remove(logFile); err != nil && !os.IsNotExist(err) {
		m.logger.Debug("error removing firecracker log during cleanup", "agent_id", agentID, "path", logFile, "error", err)
	}

	// Remove the agent state directory itself.
	if err := os.RemoveAll(stateDir); err != nil && !os.IsNotExist(err) {
		m.logger.Warn("failed to remove agent state directory", "dir", stateDir, "error", err)
	}

	m.logger.Info("cleaned up agent disk artifacts", "agent_id", agentID, "state_dir", stateDir)
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

// resolveResources extracts memory (in MB), vCPU count, and disk size (in MB)
// from the agent manifest, using defaults if not specified.
// Defaults: 512 MiB memory, 1 vCPU, 1 GiB disk.
func (m *Manager) resolveResources(agent *types.AgentManifest) (memoryMB int, vcpus int, diskMB int, err error) {
	memoryMB = 512
	vcpus = 1
	diskMB = 1024

	if agent.Spec.Resources.Memory != "" {
		memBytes, parseErr := config.ParseMemory(agent.Spec.Resources.Memory)
		if parseErr != nil {
			return 0, 0, 0, fmt.Errorf("parsing memory %q: %w", agent.Spec.Resources.Memory, parseErr)
		}
		memoryMB = int(memBytes / (1024 * 1024))
		if memoryMB <= 0 {
			return 0, 0, 0, fmt.Errorf("memory %d bytes resolves to 0 MiB", memBytes)
		}
	}

	if agent.Spec.Resources.VCPUs > 0 {
		vcpus = agent.Spec.Resources.VCPUs
	}

	if agent.Spec.Resources.Disk != "" {
		diskBytes, parseErr := config.ParseDiskSize(agent.Spec.Resources.Disk)
		if parseErr != nil {
			return 0, 0, 0, fmt.Errorf("parsing disk %q: %w", agent.Spec.Resources.Disk, parseErr)
		}
		diskMB = int(diskBytes / (1024 * 1024))
		if diskMB <= 0 {
			return 0, 0, 0, fmt.Errorf("disk %d bytes resolves to 0 MiB", diskBytes)
		}
	}

	if vcpus > 256 {
		return 0, 0, 0, fmt.Errorf("VCPUs %d exceeds maximum of 256", vcpus)
	}
	if memoryMB > 1024*1024 {
		return 0, 0, 0, fmt.Errorf("memory %d MiB exceeds maximum of 1 TiB", memoryMB)
	}
	if diskMB > 10*1024*1024 {
		return 0, 0, 0, fmt.Errorf("disk %d MiB exceeds maximum of 10 TiB", diskMB)
	}

	return memoryMB, vcpus, diskMB, nil
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
func createAgentDriveImage(ctx context.Context, agentDir, imgPath string, diskMB int) error {
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

	// Enforce an upper bound: the configured disk size is the hard limit.
	maxDriveBytes := int64(diskMB) * 1024 * 1024
	if totalSize > maxDriveBytes {
		return fmt.Errorf("agent directory too large for drive image: %d bytes (max %d bytes from spec.resources.disk)", totalSize, maxDriveBytes)
	}

	// Use the configured disk size. Ensure at least 4 MB for ext4 metadata,
	// and at least enough for directory contents plus overhead.
	imgSizeMB := int64(diskMB)
	contentSizeMB := totalSize/(1024*1024) + 4 // contents + ext4 overhead
	if contentSizeMB > imgSizeMB {
		imgSizeMB = contentSizeMB
	}
	if imgSizeMB < 4 {
		imgSizeMB = 4
	}

	// mkfs.ext4 -d <source_dir> populates the filesystem from the directory
	// without requiring a loop mount. The size argument is in 1K blocks.
	sizeBlocks := fmt.Sprintf("%dk", imgSizeMB*1024)
	mkfsCtx, mkfsCancel := context.WithTimeout(ctx, 60*time.Second)
	defer mkfsCancel()
	mkfs := exec.CommandContext(mkfsCtx, mkfsPath, "-q", "-F", "-d", agentDir, imgPath, sizeBlocks)
	if out, runErr := mkfs.CombinedOutput(); runErr != nil {
		os.Remove(imgPath)
		return fmt.Errorf("mkfs.ext4 -d: %s: %w", string(out), runErr)
	}

	return nil
}

// createSharedVolumeImage creates an ext4 disk image from a shared volume's
// host directory. The image can be attached as an additional Firecracker drive.
//
// This uses mkfs.ext4 -d to populate the filesystem from the source directory
// without requiring mount/umount (which need root or CAP_SYS_ADMIN).
//
// Note: Firecracker does not support true virtiofs pass-through. Shared volumes
// are packaged as ext4 block device images. Changes inside the VM are written
// to the image, not synced back to the host directory in real time. For true
// live sharing between host and guest, a virtiofsd daemon would be needed.
func createSharedVolumeImage(ctx context.Context, hostDir, imgPath string) error {
	mkfsPath, err := exec.LookPath("mkfs.ext4")
	if err != nil {
		// mkfs.ext4 is not available (e.g. macOS). Create an empty placeholder
		// so that the rest of the startup path can proceed.
		slog.Warn("mkfs.ext4 not found in PATH; creating empty shared volume placeholder",
			"host_dir", hostDir,
			"img_path", imgPath,
		)
		f, createErr := os.Create(imgPath)
		if createErr != nil {
			return fmt.Errorf("creating shared volume placeholder: %w", createErr)
		}
		return f.Close()
	}

	// Verify the host directory exists. Use Lstat to detect symlinks.
	info, statErr := os.Lstat(hostDir)
	if statErr != nil {
		return fmt.Errorf("shared volume host path %q: %w", hostDir, statErr)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("shared volume host_path %q is a symlink, which is not allowed for security", hostDir)
	}
	if !info.IsDir() {
		return fmt.Errorf("shared volume host path %q is not a directory", hostDir)
	}

	// Walk the directory using WalkDir (not Walk) to avoid following symlinks
	// during size calculation. Reject any symlinks found inside the directory
	// because mkfs.ext4 -d would follow them, potentially packaging content
	// outside the shared volume directory.
	var totalSize int64
	walkErr := filepath.WalkDir(hostDir, func(path string, d os.DirEntry, walkEntryErr error) error {
		if walkEntryErr != nil {
			return walkEntryErr
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("shared volume directory contains symlink %q which is not allowed for security reasons", path)
		}
		if !d.IsDir() {
			fi, fiErr := d.Info()
			if fiErr != nil {
				return fiErr
			}
			totalSize += fi.Size()
		}
		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("calculating shared volume directory size: %w", walkErr)
	}

	// Enforce an upper bound on the shared volume image size (2 GB).
	const maxSharedVolSize int64 = 2 << 30 // 2 GiB
	if totalSize > maxSharedVolSize {
		return fmt.Errorf("shared volume directory too large: %d bytes (max %d bytes)", totalSize, maxSharedVolSize)
	}

	// Add padding for ext4 metadata and overhead (at least 4MB).
	imgSizeMB := (totalSize/(1024*1024) + 4)
	if imgSizeMB < 4 {
		imgSizeMB = 4
	}

	// TOCTOU guard: verify that the host directory path has not been replaced
	// by a symlink between the initial Lstat check and the mkfs.ext4 call.
	resolvedDir, evalErr := filepath.EvalSymlinks(hostDir)
	if evalErr != nil {
		return fmt.Errorf("evaluating symlinks on shared volume host path %q: %w", hostDir, evalErr)
	}
	absHostDir, absErr := filepath.Abs(hostDir)
	if absErr != nil {
		return fmt.Errorf("resolving absolute path for shared volume host path %q: %w", hostDir, absErr)
	}
	if resolvedDir != absHostDir {
		return fmt.Errorf("shared volume host path %q resolves to %q (expected %q): possible symlink TOCTOU attack", hostDir, resolvedDir, absHostDir)
	}

	sizeBlocks := fmt.Sprintf("%dk", imgSizeMB*1024)
	mkfsCtx, mkfsCancel := context.WithTimeout(ctx, 60*time.Second)
	defer mkfsCancel()
	mkfs := exec.CommandContext(mkfsCtx, mkfsPath, "-q", "-F", "-d", hostDir, imgPath, sizeBlocks)
	if out, runErr := mkfs.CombinedOutput(); runErr != nil {
		os.Remove(imgPath)
		return fmt.Errorf("mkfs.ext4 -d (shared volume): %s: %w", string(out), runErr)
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
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
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
