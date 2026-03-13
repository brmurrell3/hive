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
		f.mu.Unlock()
		// Close the per-VM log file now that the process has exited.
		if fp.logFile != nil {
			fp.logFile.Close()
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
// path under the lock. Returns nil if no process is tracked.
func (f *FirecrackerHypervisor) claimProcess(socketPath string) *firecrackerProcess {
	f.mu.Lock()
	defer f.mu.Unlock()
	fp := f.processes[socketPath]
	if fp != nil {
		delete(f.processes, socketPath)
	}
	return fp
}

// CreateVM spawns the Firecracker process and configures the VM via the API
// socket. The VM is configured but not started (use StartVM for that).
// Returns the process PID on success.
func (f *FirecrackerHypervisor) CreateVM(cfg VMConfig) (int, error) {
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

	// Wait for the API socket to appear.
	if err := waitForSocket(cfg.SocketPath, 5*time.Second); err != nil {
		// Remove from map so the reaper goroutine knows we're abandoning it,
		// then kill and wait for the goroutine to finish.
		f.mu.Lock()
		if current, ok := f.processes[cfg.SocketPath]; ok && current == fp {
			delete(f.processes, cfg.SocketPath)
		}
		f.mu.Unlock()
		_ = cmd.Process.Kill()
		// waitForReap waits for the reaper goroutine which closes fp.logFile,
		// so we must not close logFile again here (double-close).
		waitForReap(fp, 5*time.Second)
		return 0, fmt.Errorf("waiting for API socket: %w", err)
	}

	client := socketHTTPClient(cfg.SocketPath)
	defer client.CloseIdleConnections()

	// Configure the boot source (kernel).
	bootSource := firecrackerBootSource{
		KernelImagePath: cfg.KernelPath,
		BootArgs:        "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda ro init=/init",
	}
	if err := apiPut(client, cfg.SocketPath, "/boot-source", bootSource); err != nil {
		f.mu.Lock()
		if current, ok := f.processes[cfg.SocketPath]; ok && current == fp {
			delete(f.processes, cfg.SocketPath)
		}
		f.mu.Unlock()
		_ = cmd.Process.Kill()
		waitForReap(fp, 5*time.Second)
		return 0, fmt.Errorf("configuring boot source: %w", err)
	}

	// Configure the rootfs drive.
	rootfsDrive := firecrackerDrive{
		DriveID:      "rootfs",
		PathOnHost:   cfg.RootfsPath,
		IsRootDevice: true,
		IsReadOnly:   false,
	}
	if err := apiPut(client, cfg.SocketPath, "/drives/rootfs", rootfsDrive); err != nil {
		f.mu.Lock()
		if current, ok := f.processes[cfg.SocketPath]; ok && current == fp {
			delete(f.processes, cfg.SocketPath)
		}
		f.mu.Unlock()
		_ = cmd.Process.Kill()
		waitForReap(fp, 5*time.Second)
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
			if err := apiPut(client, cfg.SocketPath, "/drives/agent", agentDrive); err != nil {
				f.mu.Lock()
				if current, ok := f.processes[cfg.SocketPath]; ok && current == fp {
					delete(f.processes, cfg.SocketPath)
				}
				f.mu.Unlock()
				_ = cmd.Process.Kill()
				waitForReap(fp, 5*time.Second)
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
		if err := apiPut(client, cfg.SocketPath, fmt.Sprintf("/drives/%s", driveID), drive); err != nil {
			f.mu.Lock()
			if current, ok := f.processes[cfg.SocketPath]; ok && current == fp {
				delete(f.processes, cfg.SocketPath)
			}
			f.mu.Unlock()
			_ = cmd.Process.Kill()
			waitForReap(fp, 5*time.Second)
			return 0, fmt.Errorf("configuring shared volume drive %q: %w", vol.Name, err)
		}
	}

	// Configure the machine (vCPUs, memory).
	machineCfg := firecrackerMachineConfig{
		VCPUCount:  cfg.VCPUs,
		MemSizeMiB: cfg.MemoryMB,
		Smt:        false,
	}
	if err := apiPut(client, cfg.SocketPath, "/machine-config", machineCfg); err != nil {
		f.mu.Lock()
		if current, ok := f.processes[cfg.SocketPath]; ok && current == fp {
			delete(f.processes, cfg.SocketPath)
		}
		f.mu.Unlock()
		_ = cmd.Process.Kill()
		waitForReap(fp, 5*time.Second)
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
		if err := apiPut(client, cfg.SocketPath, "/network-interfaces/eth0", netIface); err != nil {
			f.mu.Lock()
			if current, ok := f.processes[cfg.SocketPath]; ok && current == fp {
				delete(f.processes, cfg.SocketPath)
			}
			f.mu.Unlock()
			_ = cmd.Process.Kill()
			waitForReap(fp, 5*time.Second)
			return 0, fmt.Errorf("configuring network interface: %w", err)
		}
	}

	// Configure vsock for host-guest communication.
	vsockPath := cfg.SocketPath + ".vsock"
	vsock := firecrackerVsock{
		GuestCID: int(cfg.CID),
		UDSPath:  vsockPath,
	}
	if err := apiPut(client, cfg.SocketPath, "/vsock", vsock); err != nil {
		f.mu.Lock()
		if current, ok := f.processes[cfg.SocketPath]; ok && current == fp {
			delete(f.processes, cfg.SocketPath)
		}
		f.mu.Unlock()
		_ = cmd.Process.Kill()
		waitForReap(fp, 5*time.Second)
		return 0, fmt.Errorf("configuring vsock: %w", err)
	}

	f.logger.Info("Firecracker VM configured",
		"agent_id", cfg.AgentID,
		"pid", pid,
	)

	return pid, nil
}

// StartVM boots a previously created VM by sending the InstanceStart action
// via the Firecracker API socket.
func (f *FirecrackerHypervisor) StartVM(socketPath string) error {
	f.logger.Info("starting Firecracker VM", "socket", socketPath)

	client := socketHTTPClient(socketPath)
	action := firecrackerAction{ActionType: "InstanceStart"}

	if err := apiPut(client, socketPath, "/actions", action); err != nil {
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
	fp := f.claimProcess(socketPath)

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
			cleanupSocket(socketPath)
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
		exited := waitForProcessExit(pid, 5*time.Second)
		if !exited {
			f.logger.Warn("VM process did not exit after SIGTERM, sending SIGKILL",
				"pid", pid,
			)
			if err := proc.Signal(syscall.SIGKILL); err != nil && !isProcessGone(err) {
				return fmt.Errorf("sending SIGKILL to PID %d: %w", pid, err)
			}
			waitForProcessExit(pid, 2*time.Second)
		}
	}

	cleanupSocket(socketPath)
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
	fp := f.claimProcess(socketPath)

	if pid > 0 {
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
		}
	}

	cleanupSocket(socketPath)
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
		return true // process exists and we can signal it
	}
	// EPERM means the process exists but we can't signal it (different user).
	if errors.Is(err, syscall.EPERM) {
		return true
	}
	return false // ESRCH or other error means process is gone
}

// socketHTTPClient creates an HTTP client that communicates over a Unix socket.
func socketHTTPClient(socketPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
		Timeout: 10 * time.Second,
	}
}

// apiPut sends a PUT request to the Firecracker API over the Unix socket.
func apiPut(client *http.Client, socketPath string, path string, body interface{}) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshaling request body: %w", err)
	}

	req, err := http.NewRequest(http.MethodPut, "http://localhost"+path, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("sending request to %s: %w", path, err)
	}
	defer resp.Body.Close()

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
// given timeout.
func waitForSocket(socketPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socketPath); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("socket %s did not appear within %s", socketPath, timeout)
}

// waitForProcessExit polls for process exit up to the given timeout.
// Returns true if the process exited, false if the timeout was reached.
func waitForProcessExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		proc, err := os.FindProcess(pid)
		if err != nil {
			return true
		}
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			return true
		}
		time.Sleep(100 * time.Millisecond)
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
	return err.Error() == "os: process already finished"
}

// deterministicMAC generates a deterministic locally-administered MAC address
// from an identifier string. The MAC uses the 06:XX:XX:XX:XX:XX pattern where
// 06 sets the locally-administered + unicast bits, and the remaining 5 bytes
// are derived from a SHA-256 hash of the identifier.
func deterministicMAC(identifier string) string {
	h := sha256.Sum256([]byte(identifier))
	return fmt.Sprintf("06:%02x:%02x:%02x:%02x:%02x", h[0], h[1], h[2], h[3], h[4])
}

// cleanupSocket removes the API socket and vsock files.
func cleanupSocket(socketPath string) {
	os.Remove(socketPath)
	os.Remove(socketPath + ".vsock")
	// Clean up port-specific vsock UDS files (e.g., firecracker.sock.vsock_4222)
	// that Firecracker creates when guests connect via AF_VSOCK.
	matches, globErr := filepath.Glob(socketPath + ".vsock_*")
	if globErr != nil {
		slog.Warn("filepath.Glob failed during socket cleanup", "pattern", socketPath+".vsock_*", "error", globErr)
	}
	for _, m := range matches {
		os.Remove(m)
	}
}
