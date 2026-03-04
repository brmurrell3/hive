package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
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
}

// NewFirecrackerHypervisor creates a new FirecrackerHypervisor.
// firecrackerBin is the path to the firecracker binary (defaults to "firecracker"
// which relies on $PATH).
func NewFirecrackerHypervisor(firecrackerBin string, logger *slog.Logger) *FirecrackerHypervisor {
	if firecrackerBin == "" {
		firecrackerBin = "firecracker"
	}
	return &FirecrackerHypervisor{
		firecrackerBin: firecrackerBin,
		logger:         logger,
	}
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

// firecrackerVsock is the vsock device configuration.
type firecrackerVsock struct {
	GuestCID int    `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

// firecrackerAction is an instance action (e.g., InstanceStart).
type firecrackerAction struct {
	ActionType string `json:"action_type"`
}

// CreateVM spawns the Firecracker process and configures the VM via the API
// socket. The VM is configured but not started (use StartVM for that).
func (f *FirecrackerHypervisor) CreateVM(cfg VMConfig) error {
	f.logger.Info("creating Firecracker VM",
		"agent_id", cfg.AgentID,
		"socket", cfg.SocketPath,
		"memory_mb", cfg.MemoryMB,
		"vcpus", cfg.VCPUs,
		"cid", cfg.CID,
	)

	// Remove stale socket if it exists.
	os.Remove(cfg.SocketPath)

	// Spawn the Firecracker process with the API socket.
	cmd := exec.Command(f.firecrackerBin, "--api-sock", cfg.SocketPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting firecracker process: %w", err)
	}

	f.logger.Info("firecracker process started",
		"agent_id", cfg.AgentID,
		"pid", cmd.Process.Pid,
	)

	// Wait for the API socket to appear.
	if err := waitForSocket(cfg.SocketPath, 5*time.Second); err != nil {
		// Kill the process if the socket never appeared.
		_ = cmd.Process.Kill()
		return fmt.Errorf("waiting for API socket: %w", err)
	}

	client := socketHTTPClient(cfg.SocketPath)

	// Configure the boot source (kernel).
	bootSource := firecrackerBootSource{
		KernelImagePath: cfg.KernelPath,
		BootArgs:        "console=ttyS0 reboot=k panic=1 pci=off",
	}
	if err := apiPut(client, cfg.SocketPath, "/boot-source", bootSource); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("configuring boot source: %w", err)
	}

	// Configure the rootfs drive.
	rootfsDrive := firecrackerDrive{
		DriveID:      "rootfs",
		PathOnHost:   cfg.RootfsPath,
		IsRootDevice: true,
		IsReadOnly:   false,
	}
	if err := apiPut(client, cfg.SocketPath, "/drives/rootfs", rootfsDrive); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("configuring rootfs drive: %w", err)
	}

	// If an agent directory is provided, attach it as a secondary drive.
	if cfg.AgentDir != "" {
		if _, err := os.Stat(cfg.AgentDir); err == nil {
			agentDrive := firecrackerDrive{
				DriveID:      "agent",
				PathOnHost:   cfg.AgentDir,
				IsRootDevice: false,
				IsReadOnly:   true,
			}
			if err := apiPut(client, cfg.SocketPath, "/drives/agent", agentDrive); err != nil {
				_ = cmd.Process.Kill()
				return fmt.Errorf("configuring agent drive: %w", err)
			}
		}
	}

	// Configure the machine (vCPUs, memory).
	machineCfg := firecrackerMachineConfig{
		VCPUCount:  cfg.VCPUs,
		MemSizeMiB: cfg.MemoryMB,
		Smt:        false,
	}
	if err := apiPut(client, cfg.SocketPath, "/machine-config", machineCfg); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("configuring machine: %w", err)
	}

	// Configure vsock for host-guest communication.
	vsockPath := cfg.SocketPath + ".vsock"
	vsock := firecrackerVsock{
		GuestCID: int(cfg.CID),
		UDSPath:  vsockPath,
	}
	if err := apiPut(client, cfg.SocketPath, "/vsock", vsock); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("configuring vsock: %w", err)
	}

	f.logger.Info("Firecracker VM configured",
		"agent_id", cfg.AgentID,
		"pid", cmd.Process.Pid,
	)

	return nil
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
// time, SIGKILL is sent.
func (f *FirecrackerHypervisor) StopVM(socketPath string, pid int) error {
	if pid <= 0 {
		return fmt.Errorf("invalid PID %d for VM at %s", pid, socketPath)
	}

	f.logger.Info("stopping Firecracker VM", "socket", socketPath, "pid", pid)

	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("finding process %d: %w", pid, err)
	}

	// Send SIGTERM for graceful shutdown.
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		// Process may already be gone.
		if isProcessGone(err) {
			f.logger.Info("VM process already exited", "pid", pid)
			cleanupSocket(socketPath)
			return nil
		}
		return fmt.Errorf("sending SIGTERM to PID %d: %w", pid, err)
	}

	// Wait up to 5 seconds for the process to exit.
	exited := waitForProcessExit(pid, 5*time.Second)
	if !exited {
		f.logger.Warn("VM process did not exit after SIGTERM, sending SIGKILL",
			"pid", pid,
		)
		if err := proc.Signal(syscall.SIGKILL); err != nil && !isProcessGone(err) {
			return fmt.Errorf("sending SIGKILL to PID %d: %w", pid, err)
		}
		// Wait a short time for SIGKILL to take effect.
		waitForProcessExit(pid, 2*time.Second)
	}

	cleanupSocket(socketPath)
	f.logger.Info("Firecracker VM stopped", "socket", socketPath, "pid", pid)
	return nil
}

// DestroyVM forcefully kills the VM process and cleans up the socket.
func (f *FirecrackerHypervisor) DestroyVM(socketPath string, pid int) error {
	f.logger.Info("destroying Firecracker VM", "socket", socketPath, "pid", pid)

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
	}

	cleanupSocket(socketPath)
	return nil
}

// IsRunning checks whether a process with the given PID exists by sending
// signal 0 (which checks for process existence without actually sending a signal).
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
	return err == nil
}

// socketHTTPClient creates an HTTP client that communicates over a Unix socket.
func socketHTTPClient(socketPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
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
		respBody.ReadFrom(resp.Body)
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
	return err == os.ErrProcessDone || err.Error() == "os: process already finished"
}

// cleanupSocket removes the API socket and vsock files.
func cleanupSocket(socketPath string) {
	os.Remove(socketPath)
	os.Remove(socketPath + ".vsock")
}
