// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package vm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

// FirecrackerHypervisor implements the Hypervisor interface using the real
// Firecracker binary. It communicates with Firecracker via its Unix socket
// HTTP API.
//
// This code compiles on all platforms but requires KVM (/dev/kvm) to actually
// run. On macOS it will compile but all operations will fail at runtime.
type FirecrackerHypervisor struct {
	firecrackerBin string
	logger         *slog.Logger

	mu        sync.Mutex
	processes map[string]*firecrackerProcess // socket path -> process
}

// firecrackerProcess tracks a running Firecracker child process and provides
// a channel that is closed once cmd.Wait() has returned (i.e. the OS has
// reaped the process and freed its PID slot).
type firecrackerProcess struct {
	cmd     *exec.Cmd
	done    chan struct{} // closed by the background reaper goroutine
	logFile *os.File      // per-VM log file for stdout/stderr; closed on stop/destroy
}

// NewFirecrackerHypervisor creates a new FirecrackerHypervisor.
// firecrackerBin is the path to the firecracker binary (defaults to "firecracker"
// which relies on $PATH).
//
// Pre-flight checks are performed at construction time to provide clear error
// messages when /dev/kvm is missing or the firecracker binary is not installed.
func NewFirecrackerHypervisor(firecrackerBin string, logger *slog.Logger) (*FirecrackerHypervisor, error) {
	// Pre-flight check: /dev/kvm must exist and be accessible.
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return nil, fmt.Errorf("pre-flight check failed: /dev/kvm is not available (KVM is required for Firecracker VMs): %w", err)
	}

	// Pre-flight check: firecracker binary must be in PATH (or at the given path).
	if firecrackerBin == "" {
		firecrackerBin = "firecracker"
	}
	resolvedBin, err := exec.LookPath(firecrackerBin)
	if err != nil {
		return nil, fmt.Errorf("pre-flight check failed: firecracker binary %q not found in PATH: %w", firecrackerBin, err)
	}

	// Pre-flight check: detect Firecracker version and warn if below minimum.
	checkFirecrackerVersion(resolvedBin, logger)

	return &FirecrackerHypervisor{
		firecrackerBin: resolvedBin,
		logger:         logger,
		processes:      make(map[string]*firecrackerProcess),
	}, nil
}

// firecrackerBootSource is the kernel boot source configuration.
type firecrackerBootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args"`
}

// firecrackerDrive is a block device configuration.
type firecrackerDrive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

// firecrackerMachineConfig is the VM machine configuration.
type firecrackerMachineConfig struct {
	VCPUCount  int  `json:"vcpu_count"`
	MemSizeMiB int  `json:"mem_size_mib"`
	Smt        bool `json:"smt"`
}

// firecrackerNetworkInterface is the network interface configuration.
type firecrackerNetworkInterface struct {
	IfaceID     string `json:"iface_id"`
	GuestMAC    string `json:"guest_mac"`
	HostDevName string `json:"host_dev_name"`
}

// firecrackerVsock is the vsock device configuration.
type firecrackerVsock struct {
	GuestCID int    `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

// firecrackerAction is an instance action (e.g., InstanceStart).
type firecrackerAction struct {
	ActionType string `json:"action_type"`
}

// startReaper registers cmd in the processes map and starts a background
// goroutine that calls cmd.Wait(). The goroutine closes fp.done when Wait
// returns, signalling that the OS has fully reaped the process. If a log file
// is provided it is stored in the process record and closed by the reaper
// goroutine after the process exits. The caller must hold f.mu while calling
// this function.
func (f *FirecrackerHypervisor) startReaper(socketPath string, cmd *exec.Cmd, logFile *os.File) *firecrackerProcess {
	fp := &firecrackerProcess{
		cmd:     cmd,
		done:    make(chan struct{}),
		logFile: logFile,
	}
	f.processes[socketPath] = fp

	go func() {
		if waitErr := cmd.Wait(); waitErr != nil {
			f.logger.Warn("firecracker process exited with error",
				"socket", socketPath,
				"error", waitErr,
			)
		}
		f.mu.Lock()
		// Only delete the entry if it still points to this process (it may
		// have been replaced or removed by StopVM/DestroyVM already).
		if current, ok := f.processes[socketPath]; ok && current == fp {
			delete(f.processes, socketPath)
		}
		// FC-H2: Read logFile under lock to avoid race with claimProcess
		// which sets fp.logFile = nil when it claims ownership.
		lf := fp.logFile
		fp.logFile = nil
		f.mu.Unlock()
		// Close the per-VM log file now that the process has exited.
		if lf != nil {
			lf.Close()
		}
		close(fp.done)
	}()

	return fp
}

// waitForReap waits for the background reaper goroutine to call cmd.Wait().
// It blocks until the process has been reaped or the timeout elapses.
func waitForReap(fp *firecrackerProcess, timeout time.Duration) {
	select {
	case <-fp.done:
	case <-time.After(timeout):
	}
}

// claimProcess removes and returns the tracked process for the given socket
// path under the lock. Returns nil for both values if no process is tracked.
// FC-C1: Returns the log file pointer along with the process. The caller is
// responsible for closing the log file after waitForReap completes, which
// prevents the log FD leak that occurred when claimProcess set logFile=nil
// and the reaper subsequently found logFile already nil.
func (f *FirecrackerHypervisor) claimProcess(socketPath string) (*firecrackerProcess, *os.File) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fp := f.processes[socketPath]
	if fp != nil {
		delete(f.processes, socketPath)
		// FC-C1: Take ownership of the log file. Setting fp.logFile = nil
		// under the lock prevents the reaper from also closing it.
		lf := fp.logFile
		fp.logFile = nil
		return fp, lf
	}
	return nil, nil
}

// isFirecrackerProcess checks whether the process with the given PID is
// actually a Firecracker process by reading /proc/<pid>/cmdline on Linux.
// On non-Linux platforms or if /proc is unavailable, returns false and logs
// a warning. This prevents sending signals to a recycled PID that now
// belongs to an unrelated process.
func isFirecrackerProcess(pid int, logger *slog.Logger) bool {
	if runtime.GOOS != "linux" {
		logger.Warn("cannot verify process identity on non-Linux platform, skipping signal",
			"pid", pid,
			"goos", runtime.GOOS,
		)
		return false
	}
	cmdlinePath := fmt.Sprintf("/proc/%d/cmdline", pid)
	data, err := os.ReadFile(cmdlinePath)
	if err != nil {
		logger.Warn("cannot read /proc cmdline to verify process identity, skipping signal",
			"pid", pid,
			"error", err,
		)
		return false
	}
	// /proc/<pid>/cmdline uses NUL bytes as separators.
	cmdline := string(data)
	if strings.Contains(cmdline, "firecracker") {
		return true
	}
	logger.Warn("PID is not a firecracker process, skipping signal to avoid PID reuse hazard",
		"pid", pid,
		"cmdline", strings.ReplaceAll(cmdline, "\x00", " "),
	)
	return false
}

// CreateVM spawns the Firecracker process and configures the VM via the API
// socket. The VM is configured but not started (use StartVM for that).
// Returns the process PID on success.
func (f *FirecrackerHypervisor) CreateVM(ctx context.Context, cfg VMConfig) (int, error) {
	// FC-H1: Validate VMConfig inputs.
	if cfg.SocketPath == "" {
		return 0, fmt.Errorf("VMConfig.SocketPath must not be empty")
	}
	if cfg.KernelPath == "" {
		return 0, fmt.Errorf("VMConfig.KernelPath must not be empty")
	}
	if cfg.RootfsPath == "" {
		return 0, fmt.Errorf("VMConfig.RootfsPath must not be empty")
	}
	if cfg.MemoryMB <= 0 {
		return 0, fmt.Errorf("VMConfig.MemoryMB must be positive, got %d", cfg.MemoryMB)
	}
	if cfg.VCPUs <= 0 {
		return 0, fmt.Errorf("VMConfig.VCPUs must be positive, got %d", cfg.VCPUs)
	}
	if cfg.CID < 3 {
		return 0, fmt.Errorf("VMConfig.CID must be >= 3, got %d", cfg.CID)
	}

	// FC-H2b: Validate VMConfig path fields to prevent path traversal and
	// glob injection. All paths must be absolute and contain no ".." components.
	// SocketPath must also not contain glob metacharacters since it is used
	// in filepath.Glob in cleanupSocket.
	for _, pv := range []struct {
		name, path string
	}{
		{"SocketPath", cfg.SocketPath},
		{"KernelPath", cfg.KernelPath},
		{"RootfsPath", cfg.RootfsPath},
	} {
		if !filepath.IsAbs(pv.path) {
			return 0, fmt.Errorf("VMConfig.%s must be an absolute path, got %q", pv.name, pv.path)
		}
		if cleaned := filepath.Clean(pv.path); cleaned != pv.path && cleaned+"/" != pv.path {
			// Allow the path only if Clean produces the same result (no ".." resolution needed).
			if strings.Contains(pv.path, "..") {
				return 0, fmt.Errorf("VMConfig.%s must not contain '..' components, got %q", pv.name, pv.path)
			}
		}
	}
	if cfg.AgentDrivePath != "" {
		if !filepath.IsAbs(cfg.AgentDrivePath) {
			return 0, fmt.Errorf("VMConfig.AgentDrivePath must be an absolute path, got %q", cfg.AgentDrivePath)
		}
		if strings.Contains(cfg.AgentDrivePath, "..") {
			return 0, fmt.Errorf("VMConfig.AgentDrivePath must not contain '..' components, got %q", cfg.AgentDrivePath)
		}
	}
	// SocketPath is used in filepath.Glob (cleanupSocket), so reject glob metacharacters.
	if strings.ContainsAny(cfg.SocketPath, "*?[]") {
		return 0, fmt.Errorf("VMConfig.SocketPath must not contain glob metacharacters (*?[]), got %q", cfg.SocketPath)
	}

	f.logger.Info("creating Firecracker VM",
		"agent_id", cfg.AgentID,
		"socket", cfg.SocketPath,
		"memory_mb", cfg.MemoryMB,
		"vcpus", cfg.VCPUs,
		"cid", cfg.CID,
	)

	// Remove stale socket if it exists.
	os.Remove(cfg.SocketPath)

	// Open a per-VM log file for Firecracker's stdout/stderr. This prevents
	// interleaved output on hived's own stdout/stderr.
	logPath := filepath.Join(filepath.Dir(cfg.SocketPath), "firecracker.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640)
	if err != nil {
		return 0, fmt.Errorf("opening firecracker log file %s: %w", logPath, err)
	}

	// Spawn the Firecracker process with the API socket.
	cmd := exec.Command(f.firecrackerBin, "--api-sock", cfg.SocketPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return 0, fmt.Errorf("starting firecracker process: %w", err)
	}

	pid := cmd.Process.Pid
	f.logger.Info("firecracker process started",
		"agent_id", cfg.AgentID,
		"pid", pid,
	)

	// Register the process and start the background reaper goroutine. The
	// goroutine is the sole caller of cmd.Wait(), which prevents zombie
	// accumulation regardless of how the process exits. The reaper also
	// closes the log file when the process exits.
	f.mu.Lock()
	fp := f.startReaper(cfg.SocketPath, cmd, logFile)
	f.mu.Unlock()

	// FC-H2: Deferred cleanup of the socket on error paths. Set to true now
	// that the process has started; cleared to false on success.
	cleanupNeeded := true
	defer func() {
		if cleanupNeeded {
			f.cleanupSocket(cfg.SocketPath)
		}
	}()

	// killAndReap is a helper to kill the process and wait for the reaper
	// on any configuration error path.
	killAndReap := func() {
		f.mu.Lock()
		if current, ok := f.processes[cfg.SocketPath]; ok && current == fp {
			delete(f.processes, cfg.SocketPath)
		}
		f.mu.Unlock()
		_ = cmd.Process.Kill()
		// waitForReap waits for the reaper goroutine which closes fp.logFile,
		// so we must not close logFile again here (double-close).
		waitForReap(fp, 5*time.Second)
	}

	// Wait for the API socket to appear.
	if err := waitForSocket(ctx, cfg.SocketPath, 5*time.Second); err != nil {
		killAndReap()
		return 0, fmt.Errorf("waiting for API socket: %w", err)
	}

	client := socketHTTPClient(cfg.SocketPath)
	defer client.CloseIdleConnections()

	// Configure the boot source (kernel).
	bootSource := firecrackerBootSource{
		KernelImagePath: cfg.KernelPath,
		BootArgs:        "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda ro init=/init",
	}
	if err := apiPut(ctx, client, cfg.SocketPath, "/boot-source", bootSource); err != nil {
		killAndReap()
		return 0, fmt.Errorf("configuring boot source: %w", err)
	}

	// Configure the rootfs drive.
	rootfsDrive := firecrackerDrive{
		DriveID:      "rootfs",
		PathOnHost:   cfg.RootfsPath,
		IsRootDevice: true,
		IsReadOnly:   false,
	}
	if err := apiPut(ctx, client, cfg.SocketPath, "/drives/rootfs", rootfsDrive); err != nil {
		killAndReap()
		return 0, fmt.Errorf("configuring rootfs drive: %w", err)
	}

	// If an agent drive image is provided, attach it as a secondary drive.
	// This must be a block device image (.img), not a directory.
	if cfg.AgentDrivePath != "" {
		if _, err := os.Stat(cfg.AgentDrivePath); err == nil {
			agentDrive := firecrackerDrive{
				DriveID:      "agent",
				PathOnHost:   cfg.AgentDrivePath,
				IsRootDevice: false,
				IsReadOnly:   false,
			}
			if err := apiPut(ctx, client, cfg.SocketPath, "/drives/agent", agentDrive); err != nil {
				killAndReap()
				return 0, fmt.Errorf("configuring agent drive: %w", err)
			}
		}
	}

	// Configure shared volume drives. Each volume is an ext4 image created
	// from the team's shared directory. They appear as /dev/vdc, /dev/vdd, etc.
	// inside the guest (after rootfs on /dev/vda and agent drive on /dev/vdb).
	for i, vol := range cfg.Volumes {
		driveID := fmt.Sprintf("shared-%d", i)
		drive := firecrackerDrive{
			DriveID:      driveID,
			PathOnHost:   vol.HostPath,
			IsRootDevice: false,
			IsReadOnly:   vol.ReadOnly,
		}
		if err := apiPut(ctx, client, cfg.SocketPath, fmt.Sprintf("/drives/%s", driveID), drive); err != nil {
			killAndReap()
			return 0, fmt.Errorf("configuring shared volume drive %q: %w", vol.Name, err)
		}
	}

	// Configure the machine (vCPUs, memory).
	machineCfg := firecrackerMachineConfig{
		VCPUCount:  cfg.VCPUs,
		MemSizeMiB: cfg.MemoryMB,
		Smt:        false,
	}
	if err := apiPut(ctx, client, cfg.SocketPath, "/machine-config", machineCfg); err != nil {
		killAndReap()
		return 0, fmt.Errorf("configuring machine: %w", err)
	}

	// Configure the network interface if a network policy with a TAP device
	// is provided. This attaches the host TAP device to the guest as eth0 with
	// a deterministic MAC address derived from the agent ID.
	if cfg.NetworkPolicy != nil && cfg.NetworkPolicy.TapDevice != "" {
		netIface := firecrackerNetworkInterface{
			IfaceID:     "eth0",
			GuestMAC:    deterministicMAC(cfg.AgentID),
			HostDevName: cfg.NetworkPolicy.TapDevice,
		}
		if err := apiPut(ctx, client, cfg.SocketPath, "/network-interfaces/eth0", netIface); err != nil {
			killAndReap()
			return 0, fmt.Errorf("configuring network interface: %w", err)
		}
	}

	// Configure vsock for host-guest communication.
	vsockPath := cfg.SocketPath + ".vsock"
	vsock := firecrackerVsock{
		GuestCID: int(cfg.CID),
		UDSPath:  vsockPath,
	}
	if err := apiPut(ctx, client, cfg.SocketPath, "/vsock", vsock); err != nil {
		killAndReap()
		return 0, fmt.Errorf("configuring vsock: %w", err)
	}

	// FC-H2: All configuration succeeded; clear the cleanup flag.
	cleanupNeeded = false

	f.logger.Info("Firecracker VM configured",
		"agent_id", cfg.AgentID,
		"pid", pid,
	)

	return pid, nil
}

// StartVM boots a previously created VM by sending the InstanceStart action
// via the Firecracker API socket.
// FC-C2: Accepts context.Context so the caller can propagate cancellation/timeouts
// instead of using an uncancellable context.Background().
func (f *FirecrackerHypervisor) StartVM(ctx context.Context, socketPath string) error {
	f.logger.Info("starting Firecracker VM", "socket", socketPath)

	client := socketHTTPClient(socketPath)
	defer client.CloseIdleConnections()
	action := firecrackerAction{ActionType: "InstanceStart"}

	if err := apiPut(ctx, client, socketPath, "/actions", action); err != nil {
		return fmt.Errorf("sending InstanceStart action: %w", err)
	}

	f.logger.Info("Firecracker VM started", "socket", socketPath)
	return nil
}

// StopVM gracefully stops a running VM. It sends SIGTERM to the Firecracker
// process and waits up to 5 seconds for it to exit. If it does not exit in
// time, SIGKILL is sent. The background reaper goroutine (started in CreateVM)
// calls cmd.Wait() to prevent zombie processes.
func (f *FirecrackerHypervisor) StopVM(socketPath string, pid int) error {
	if pid <= 0 {
		return fmt.Errorf("invalid PID %d for VM at %s", pid, socketPath)
	}

	f.logger.Info("stopping Firecracker VM", "socket", socketPath, "pid", pid)

	// Claim the tracked process. After this point the reaper goroutine will
	// not remove the map entry, but it will still call cmd.Wait() and close
	// fp.done when the process exits.
	// FC-C1: claimProcess now returns the log file; caller closes it after reap.
	fp, logFile := f.claimProcess(socketPath)
	defer func() {
		if logFile != nil {
			logFile.Close()
		}
	}()

	// FC-C1: When we have no tracked process handle, verify that the PID
	// still belongs to a firecracker process before sending any signal.
	// This prevents signaling an unrelated process after PID reuse.
	if fp == nil && !isFirecrackerProcess(pid, f.logger) {
		f.logger.Warn("skipping signal: cannot confirm PID is a firecracker process",
			"pid", pid,
			"socket", socketPath,
		)
		f.cleanupSocket(socketPath)
		return nil
	}

	// FC-H1: If the process has already exited (done channel closed), skip
	// sending SIGTERM to avoid signaling a potentially recycled PID.
	if fp != nil {
		select {
		case <-fp.done:
			f.logger.Info("VM process already exited before SIGTERM", "pid", pid)
			f.cleanupSocket(socketPath)
			return nil
		default:
		}
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("finding process %d: %w", pid, err)
	}

	// Send SIGTERM for graceful shutdown.
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		// Process may already be gone.
		if isProcessGone(err) {
			f.logger.Info("VM process already exited", "pid", pid)
			if fp != nil {
				waitForReap(fp, 5*time.Second)
			}
			f.cleanupSocket(socketPath)
			return nil
		}
		return fmt.Errorf("sending SIGTERM to PID %d: %w", pid, err)
	}

	// Wait up to 5 seconds for the reaper goroutine to confirm the process
	// exited (i.e. cmd.Wait() returned). Fall back to SIGKILL if needed.
	if fp != nil {
		select {
		case <-fp.done:
			// Process exited cleanly within the grace period.
		case <-time.After(5 * time.Second):
			f.logger.Warn("VM process did not exit after SIGTERM, sending SIGKILL",
				"pid", pid,
			)
			if err := proc.Signal(syscall.SIGKILL); err != nil && !isProcessGone(err) {
				return fmt.Errorf("sending SIGKILL to PID %d: %w", pid, err)
			}
			// Wait for the reaper to confirm SIGKILL took effect.
			waitForReap(fp, 2*time.Second)
		}
	} else {
		// No tracked process (e.g. hived restarted and lost the cmd handle).
		// Fall back to polling-based wait, accepting that we cannot call Wait().
		// Use context.Background() for the cleanup path because the caller's
		// context may already be cancelled during shutdown, and we still need
		// to wait for the process to exit before cleaning up.
		exited := waitForProcessExit(context.Background(), pid, 5*time.Second)
		if !exited {
			f.logger.Warn("VM process did not exit after SIGTERM, sending SIGKILL",
				"pid", pid,
			)
			if err := proc.Signal(syscall.SIGKILL); err != nil && !isProcessGone(err) {
				return fmt.Errorf("sending SIGKILL to PID %d: %w", pid, err)
			}
			waitForProcessExit(context.Background(), pid, 2*time.Second)
		}
	}

	f.cleanupSocket(socketPath)
	f.logger.Info("Firecracker VM stopped", "socket", socketPath, "pid", pid)
	return nil
}

// DestroyVM forcefully kills the VM process and cleans up the socket. The
// background reaper goroutine (started in CreateVM) calls cmd.Wait() to
// prevent zombie processes.
func (f *FirecrackerHypervisor) DestroyVM(socketPath string, pid int) error {
	f.logger.Info("destroying Firecracker VM", "socket", socketPath, "pid", pid)

	// Claim the tracked process before sending any signal so the reaper
	// goroutine does not race with our map removal.
	// FC-C1: claimProcess now returns the log file; caller closes it after reap.
	fp, logFile := f.claimProcess(socketPath)
	defer func() {
		if logFile != nil {
			logFile.Close()
		}
	}()

	if pid > 0 {
		// FC-C1: When we have no tracked process handle, verify that the
		// PID still belongs to a firecracker process before sending SIGKILL.
		if fp == nil && !isFirecrackerProcess(pid, f.logger) {
			f.logger.Warn("skipping SIGKILL: cannot confirm PID is a firecracker process",
				"pid", pid,
				"socket", socketPath,
			)
		} else {
			proc, err := os.FindProcess(pid)
			if err == nil {
				if killErr := proc.Signal(syscall.SIGKILL); killErr != nil && !isProcessGone(killErr) {
					f.logger.Warn("error sending SIGKILL during destroy",
						"pid", pid,
						"error", killErr,
					)
				}
			}

			// Wait for the reaper goroutine to confirm the process has been
			// reaped, preventing zombie accumulation.
			if fp != nil {
				waitForReap(fp, 5*time.Second)
			} else {
				// FC-H4: When fp is nil, wait for process exit after SIGKILL
				// to avoid leaving zombies or proceeding before cleanup.
				// Use context.Background() because destroy is a force-kill
				// cleanup path and the caller's context may be cancelled.
				waitForProcessExit(context.Background(), pid, 5*time.Second)
			}
		}
	}

	f.cleanupSocket(socketPath)
	return nil
}

// IsRunning checks whether a process with the given PID exists by sending
// signal 0 (which checks for process existence without actually sending a signal).
// Returns true if the process exists, including when we get EPERM (process
// exists but belongs to another user).
func (f *FirecrackerHypervisor) IsRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 tests for process existence.
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		// FC-C3: Process exists, but verify it's actually a Firecracker
		// process to guard against PID reuse by an unrelated process.
		if !isFirecrackerProcess(pid, f.logger) {
			return false
		}
		return true
	}
	// EPERM means the process exists but we can't signal it (different user).
	if errors.Is(err, syscall.EPERM) {
		// FC-C3: Cannot verify identity via /proc when EPERM, but the
		// process exists. Attempt identity check; if it fails, treat as
		// not running to be safe against PID reuse.
		if !isFirecrackerProcess(pid, f.logger) {
			return false
		}
		return true
	}
	return false // ESRCH or other error means process is gone
}

// socketHTTPClient creates an HTTP client that communicates over a Unix socket.
// FC-H2: MaxIdleConnsPerHost and MaxConnsPerHost are set to bound the transport
// pool, preventing unbounded connection accumulation to the local socket.
func socketHTTPClient(socketPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
			MaxIdleConnsPerHost: 1,
			MaxConnsPerHost:     2,
		},
		Timeout: 10 * time.Second,
	}
}

// apiPut sends a PUT request to the Firecracker API over the Unix socket.
// FC-H7: Accepts context.Context to propagate cancellation/timeouts.
func apiPut(ctx context.Context, client *http.Client, socketPath string, path string, body interface{}) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshaling request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://localhost"+path, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("sending request to %s: %w", path, err)
	}
	defer func() {
		// FC-S8: Drain the response body remainder before closing so the
		// underlying connection can be reused by the transport pool.
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var respBody bytes.Buffer
		if _, readErr := respBody.ReadFrom(io.LimitReader(resp.Body, 1<<20)); readErr != nil {
			return fmt.Errorf("API %s returned status %d (failed to read body: %w)", path, resp.StatusCode, readErr)
		}
		return fmt.Errorf("API %s returned status %d: %s", path, resp.StatusCode, respBody.String())
	}

	return nil
}

// waitForSocket polls for the existence of a Unix socket file up to the
// given timeout. The context is checked before each sleep so that callers
// can cancel the wait early.
func waitForSocket(ctx context.Context, socketPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socketPath); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled while waiting for socket %s: %w", socketPath, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fmt.Errorf("socket %s did not appear within %s", socketPath, timeout)
}

// waitForProcessExit polls for process exit up to the given timeout.
// Returns true if the process exited, false if the timeout was reached or the
// context was cancelled. The context is checked before each sleep so that
// callers can cancel the wait early.
// FC-C2: EPERM is treated as "still alive" (consistent with IsRunning),
// since it means the process exists but belongs to another user.
func waitForProcessExit(ctx context.Context, pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		proc, err := os.FindProcess(pid)
		if err != nil {
			return true
		}
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			// FC-C2: EPERM means the process exists but we can't signal it.
			// Continue polling instead of treating it as exited.
			if errors.Is(err, syscall.EPERM) {
				select {
				case <-ctx.Done():
					return false
				case <-time.After(100 * time.Millisecond):
				}
				continue
			}
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
	return false
}

// isProcessGone returns true if the error indicates the process no longer exists.
func isProcessGone(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrProcessDone) {
		return true
	}
	if errors.Is(err, syscall.ESRCH) {
		return true
	}
	return false
}

// deterministicMAC generates a deterministic locally-administered MAC address
// from an identifier string. The MAC uses the 06:XX:XX:XX:XX:XX pattern where
// 06 sets the locally-administered + unicast bits, and the remaining 5 bytes
// are derived from a SHA-256 hash of the identifier.
func deterministicMAC(identifier string) string {
	h := sha256.Sum256([]byte(identifier))
	return fmt.Sprintf("06:%02x:%02x:%02x:%02x:%02x", h[0], h[1], h[2], h[3], h[4])
}

// cleanupSocket removes the API socket and vsock files. It acquires f.mu to
// synchronize with concurrent operations that may be modifying the process map
// or accessing the socket path.
func (f *FirecrackerHypervisor) cleanupSocket(socketPath string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	os.Remove(socketPath)
	os.Remove(socketPath + ".vsock")
	// Clean up port-specific vsock UDS files (e.g., firecracker.sock.vsock_4222)
	// that Firecracker creates when guests connect via AF_VSOCK.
	expectedPrefix := socketPath + ".vsock_"
	matches, globErr := filepath.Glob(socketPath + ".vsock_*")
	if globErr != nil {
		f.logger.Warn("filepath.Glob failed during socket cleanup", "pattern", socketPath+".vsock_*", "error", globErr)
	}
	for _, m := range matches {
		// FC-H5: Verify each match starts with the expected prefix to
		// prevent removing unintended files if the glob pattern is abused.
		if !strings.HasPrefix(m, expectedPrefix) {
			f.logger.Warn("skipping unexpected glob match during socket cleanup", "match", m, "expected_prefix", expectedPrefix)
			continue
		}
		os.Remove(m)
	}
}

// minFirecrackerVersion is the minimum supported Firecracker version.
// Versions below this may lack features or bug fixes that Hive depends on.
const minFirecrackerVersion = "1.0.0"

// checkFirecrackerVersion runs "firecracker --version", parses the output,
// logs the detected version, and warns if it is below minFirecrackerVersion.
// This is a best-effort check and never returns an error; if parsing fails
// the warning is logged and execution continues.
func checkFirecrackerVersion(bin string, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, bin, "--version").CombinedOutput()
	if err != nil {
		logger.Warn("could not determine firecracker version",
			"binary", bin,
			"error", err,
		)
		return
	}

	version := parseFirecrackerVersion(string(out))
	if version == "" {
		logger.Warn("could not parse firecracker version from output",
			"binary", bin,
			"output", strings.TrimSpace(string(out)),
		)
		return
	}

	logger.Info("detected firecracker version",
		"version", version,
		"binary", bin,
	)

	if compareVersions(version, minFirecrackerVersion) < 0 {
		logger.Warn("firecracker version is below the minimum supported version",
			"detected", version,
			"minimum", minFirecrackerVersion,
		)
	}
}

// parseFirecrackerVersion extracts the version number from firecracker
// --version output. Typical output looks like:
//
//	Firecracker v1.6.0
//
// or sometimes:
//
//	Firecracker v1.6.0-dev
//
// Returns the version string without the "v" prefix, or "" if parsing fails.
func parseFirecrackerVersion(output string) string {
	// Look for a line containing a version-like pattern.
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		// Try to find "vX.Y.Z" pattern.
		for _, field := range strings.Fields(line) {
			if strings.HasPrefix(field, "v") && len(field) > 1 {
				ver := strings.TrimPrefix(field, "v")
				// Strip any pre-release suffix (e.g., "-dev", "-rc1").
				if idx := strings.IndexByte(ver, '-'); idx >= 0 {
					ver = ver[:idx]
				}
				// Basic validation: must contain at least one dot and only digits/dots.
				if strings.Contains(ver, ".") && isVersionString(ver) {
					return ver
				}
			}
		}
	}
	return ""
}

// isVersionString returns true if s contains only digits and dots and does
// not start or end with a dot.
func isVersionString(s string) bool {
	if len(s) == 0 || s[0] == '.' || s[len(s)-1] == '.' {
		return false
	}
	for _, c := range s {
		if c != '.' && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// compareVersions compares two dot-separated version strings (e.g., "1.6.0"
// vs "1.0.0"). Returns -1 if a < b, 0 if equal, 1 if a > b.
// Missing components are treated as 0 (e.g., "1.6" == "1.6.0").
func compareVersions(a, b string) int {
	aParts := strings.Split(a, ".")
	bParts := strings.Split(b, ".")

	maxLen := len(aParts)
	if len(bParts) > maxLen {
		maxLen = len(bParts)
	}

	for i := 0; i < maxLen; i++ {
		var aVal, bVal int
		if i < len(aParts) {
			for _, c := range aParts[i] {
				if c >= '0' && c <= '9' {
					aVal = aVal*10 + int(c-'0')
				}
			}
		}
		if i < len(bParts) {
			for _, c := range bParts[i] {
				if c >= '0' && c <= '9' {
					bVal = bVal*10 + int(c-'0')
				}
			}
		}
		if aVal < bVal {
			return -1
		}
		if aVal > bVal {
			return 1
		}
	}
	return 0
}
