// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

// checkResult represents the outcome of a single doctor check.
type checkResult struct {
	name   string
	status checkStatus
	detail string
	hint   string
}

type checkStatus int

const (
	checkPASS checkStatus = iota
	checkFAIL
	checkWARN
	checkSKIP
)

func (s checkStatus) String() string {
	switch s {
	case checkPASS:
		return "PASS"
	case checkFAIL:
		return "FAIL"
	case checkWARN:
		return "WARN"
	case checkSKIP:
		return "SKIP"
	default:
		return "????"
	}
}

// osLinux is the Linux operating system identifier used in runtime.GOOS checks.
const osLinux = "linux"

// doctorMinDiskGB is the minimum free disk space before a warning is issued.
const doctorMinDiskGB = 10

// doctorMinMemMB is the minimum available memory before a warning is issued (4GB).
const doctorMinMemMB = 4096

func doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check system prerequisites and environment health",
		Long: `Run a series of system checks to verify that the local environment
is properly configured for running Hive. Checks include Go version,
Firecracker availability, KVM access, nftables, Docker, rootfs/kernel
images, disk space, memory, NATS connectivity, and kernel modules.

Each check reports PASS, FAIL, WARN, or SKIP with remediation hints
for any issues found.

Exit code 1 if any check fails.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor()
		},
	}
}

func runDoctor() error {
	fmt.Println("Hive System Check")
	fmt.Println("=================")

	var results []checkResult

	results = append(results, checkGo())
	results = append(results, checkFirecracker())
	results = append(results, checkKVM())
	results = append(results, checkNftables())
	results = append(results, checkDocker())
	results = append(results, checkRootfs()...)
	results = append(results, checkKernel())
	results = append(results, checkDiskSpace())
	results = append(results, checkMemory())
	results = append(results, checkNATSConnectivity())
	results = append(results, checkVhostVsock())

	// Print results.
	for _, r := range results {
		printResult(r)
	}

	// Summary.
	var pass, fail, warn, skip int
	for _, r := range results {
		switch r.status {
		case checkPASS:
			pass++
		case checkFAIL:
			fail++
		case checkWARN:
			warn++
		case checkSKIP:
			skip++
		}
	}

	fmt.Printf("\nResult: %d passed, %d failed, %d warnings, %d skipped\n",
		pass, fail, warn, skip)

	if fail > 0 {
		return fmt.Errorf("%d check(s) failed", fail)
	}
	return nil
}

func printResult(r checkResult) {
	fmt.Printf("[%s] %s\n", colorizeCheckStatus(r.status), r.detail)
	if r.hint != "" {
		// Indent hint lines under the result.
		for _, line := range strings.Split(r.hint, "\n") {
			fmt.Printf("       -> %s\n", line)
		}
	}
}

// checkGo verifies that Go is installed and reports the version.
func checkGo() checkResult {
	out, err := exec.Command("go", "version").Output()
	if err != nil {
		return checkResult{
			name:   "go",
			status: checkWARN,
			detail: "Go not found in PATH",
			hint:   "Install Go: https://go.dev/dl/",
		}
	}
	ver := strings.TrimSpace(string(out))
	// Extract version number from "go version go1.25.8 darwin/arm64"
	parts := strings.Fields(ver)
	goVer := "unknown"
	if len(parts) >= 3 {
		goVer = strings.TrimPrefix(parts[2], "go")
	}
	return checkResult{
		name:   "go",
		status: checkPASS,
		detail: fmt.Sprintf("Go %s installed", goVer),
	}
}

// checkFirecracker verifies that the firecracker binary is available.
func checkFirecracker() checkResult {
	if runtime.GOOS != osLinux {
		return checkResult{
			name:   "firecracker",
			status: checkSKIP,
			detail: "Firecracker (Linux only, skipped on " + runtime.GOOS + ")",
		}
	}

	path, err := exec.LookPath("firecracker")
	if err != nil {
		return checkResult{
			name:   "firecracker",
			status: checkWARN,
			detail: "Firecracker binary not found in PATH",
			hint:   "Install Firecracker: https://github.com/firecracker-microvm/firecracker/releases",
		}
	}

	// Try to get the version.
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		return checkResult{
			name:   "firecracker",
			status: checkPASS,
			detail: fmt.Sprintf("Firecracker found at %s (version unknown)", path),
		}
	}

	ver := strings.TrimSpace(string(out))
	// Firecracker outputs "Firecracker v1.5.0\n..."
	if idx := strings.Index(ver, "\n"); idx > 0 {
		ver = ver[:idx]
	}
	return checkResult{
		name:   "firecracker",
		status: checkPASS,
		detail: fmt.Sprintf("Firecracker found at %s (%s)", path, ver),
	}
}

// checkKVM verifies that /dev/kvm is available.
func checkKVM() checkResult {
	if runtime.GOOS != osLinux {
		return checkResult{
			name:   "kvm",
			status: checkSKIP,
			detail: "KVM (Linux only, skipped on " + runtime.GOOS + ")",
		}
	}

	info, err := os.Stat("/dev/kvm")
	if err != nil {
		return checkResult{
			name:   "kvm",
			status: checkFAIL,
			detail: "KVM not available (/dev/kvm not found)",
			hint:   "Install KVM: sudo apt install qemu-kvm",
		}
	}

	// Check if writable.
	f, err := os.OpenFile("/dev/kvm", os.O_WRONLY, 0)
	if err != nil {
		return checkResult{
			name:   "kvm",
			status: checkFAIL,
			detail: fmt.Sprintf("KVM exists but not writable (/dev/kvm, mode %s)", info.Mode()),
			hint:   "Fix permissions: sudo chmod 666 /dev/kvm or add user to kvm group: sudo usermod -aG kvm $USER",
		}
	}
	f.Close()

	return checkResult{
		name:   "kvm",
		status: checkPASS,
		detail: "KVM available (/dev/kvm)",
	}
}

// checkNftables verifies that the nft binary is available.
func checkNftables() checkResult {
	if runtime.GOOS != osLinux {
		return checkResult{
			name:   "nftables",
			status: checkSKIP,
			detail: "nftables (Linux only, skipped on " + runtime.GOOS + ")",
		}
	}

	path, err := exec.LookPath("nft")
	if err != nil {
		return checkResult{
			name:   "nftables",
			status: checkWARN,
			detail: "nftables (nft) not found in PATH",
			hint:   "Install nftables: sudo apt install nftables",
		}
	}

	return checkResult{
		name:   "nftables",
		status: checkPASS,
		detail: fmt.Sprintf("nftables (nft) available at %s", path),
	}
}

// checkDocker verifies that Docker is installed and running.
func checkDocker() checkResult {
	_, err := exec.LookPath("docker")
	if err != nil {
		return checkResult{
			name:   "docker",
			status: checkWARN,
			detail: "Docker not found in PATH",
			hint:   "Install Docker: https://docs.docker.com/engine/install/",
		}
	}

	// Check if Docker daemon is running by attempting `docker info`.
	cmd := exec.Command("docker", "info")
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		hint := "Start Docker: sudo systemctl start docker"
		if runtime.GOOS == "darwin" {
			hint = "Start Docker Desktop from Applications"
		}
		return checkResult{
			name:   "docker",
			status: checkWARN,
			detail: "Docker not running",
			hint:   hint,
		}
	}

	return checkResult{
		name:   "docker",
		status: checkPASS,
		detail: "Docker running",
	}
}

// checkRootfs looks for a rootfs image in common locations.
func checkRootfs() []checkResult {
	// Common locations to search for rootfs images.
	searchPaths := []string{
		"/var/lib/hive/rootfs",
	}

	// Also check cluster root if available.
	absRoot, err := filepath.Abs(clusterRoot)
	if err == nil {
		searchPaths = append(searchPaths,
			filepath.Join(absRoot, ".cache"),
			filepath.Join(absRoot, "rootfs"),
		)
	}

	// Also check home directory cache.
	if home, err := os.UserHomeDir(); err == nil {
		searchPaths = append(searchPaths, filepath.Join(home, ".cache", "hive"))
	}

	// Look for rootfs images with common naming patterns.
	for _, dir := range searchPaths {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if strings.Contains(name, "rootfs") && (strings.HasSuffix(name, ".ext4") || strings.HasSuffix(name, ".img")) {
				fullPath := filepath.Join(dir, name)
				return []checkResult{{
					name:   "rootfs",
					status: checkPASS,
					detail: fmt.Sprintf("Rootfs image found at %s", fullPath),
				}}
			}
		}
	}

	if runtime.GOOS != osLinux {
		return []checkResult{{
			name:   "rootfs",
			status: checkSKIP,
			detail: "Rootfs image (Linux only, skipped on " + runtime.GOOS + ")",
		}}
	}

	return []checkResult{{
		name:   "rootfs",
		status: checkWARN,
		detail: "Rootfs image not found",
		hint:   "Build or download a rootfs image to /var/lib/hive/rootfs/ or .cache/ in the cluster root",
	}}
}

// checkKernel looks for a vmlinux kernel in common locations.
func checkKernel() checkResult {
	searchPaths := []string{
		"/var/lib/hive/rootfs/vmlinux",
		"/var/lib/hive/rootfs/vmlinux.bin",
	}

	absRoot, err := filepath.Abs(clusterRoot)
	if err == nil {
		searchPaths = append(searchPaths,
			filepath.Join(absRoot, ".cache", "vmlinux"),
			filepath.Join(absRoot, "rootfs", "vmlinux"),
		)
	}

	if home, err := os.UserHomeDir(); err == nil {
		searchPaths = append(searchPaths,
			filepath.Join(home, ".cache", "hive", "vmlinux"),
		)
	}

	for _, p := range searchPaths {
		if _, err := os.Stat(p); err == nil {
			return checkResult{
				name:   "kernel",
				status: checkPASS,
				detail: fmt.Sprintf("Kernel found at %s", p),
			}
		}
	}

	if runtime.GOOS != osLinux {
		return checkResult{
			name:   "kernel",
			status: checkSKIP,
			detail: "Kernel image (Linux only, skipped on " + runtime.GOOS + ")",
		}
	}

	return checkResult{
		name:   "kernel",
		status: checkWARN,
		detail: "Kernel image (vmlinux) not found",
		hint:   "Download a vmlinux kernel to /var/lib/hive/rootfs/ or .cache/ in the cluster root",
	}
}

// checkDiskSpace verifies there is sufficient free disk space.
func checkDiskSpace() checkResult {
	var stat unix.Statfs_t
	wd, err := os.Getwd()
	if err != nil {
		wd = "/"
	}
	if err := unix.Statfs(wd, &stat); err != nil {
		return checkResult{
			name:   "disk",
			status: checkWARN,
			detail: "Could not determine disk space",
			hint:   fmt.Sprintf("Error: %v", err),
		}
	}

	// Available space for non-root users.
	freeBytes := stat.Bavail * uint64(stat.Bsize)
	freeGB := float64(freeBytes) / (1024 * 1024 * 1024)

	if freeGB < float64(doctorMinDiskGB) {
		return checkResult{
			name:   "disk",
			status: checkWARN,
			detail: fmt.Sprintf("Disk space: %.1f GB free (< %d GB recommended)", freeGB, doctorMinDiskGB),
			hint:   "Free up disk space to ensure room for VM images and state",
		}
	}

	return checkResult{
		name:   "disk",
		status: checkPASS,
		detail: fmt.Sprintf("Disk space: %.1f GB free", freeGB),
	}
}

// checkMemory verifies there is sufficient available memory.
func checkMemory() checkResult {
	totalMB := systemMemoryMB()
	if totalMB <= 0 {
		return checkResult{
			name:   "memory",
			status: checkWARN,
			detail: "Could not determine system memory",
		}
	}

	totalGB := float64(totalMB) / 1024.0
	if totalMB < int64(doctorMinMemMB) {
		return checkResult{
			name:   "memory",
			status: checkWARN,
			detail: fmt.Sprintf("Memory: %.1f GB total (< %.0f GB recommended)", totalGB, float64(doctorMinMemMB)/1024),
			hint:   "Hive VMs require sufficient memory; consider adding RAM or reducing VM count",
		}
	}

	return checkResult{
		name:   "memory",
		status: checkPASS,
		detail: fmt.Sprintf("Memory: %.1f GB total", totalGB),
	}
}

// checkNATSConnectivity tries to connect to the NATS server if hived is running.
func checkNATSConnectivity() checkResult {
	natsURL, err := natsURLFromConfig(clusterRoot)
	if err != nil {
		return checkResult{
			name:   "nats",
			status: checkSKIP,
			detail: "NATS connectivity (could not determine NATS URL from config)",
		}
	}

	opts := []nats.Option{
		nats.Timeout(2 * time.Second),
		nats.Name("hivectl-doctor"),
	}

	if token := natsAuthToken(clusterRoot); token != "" {
		opts = append(opts, nats.Token(token))
	}

	nc, err := nats.Connect(natsURL, opts...)
	if err != nil {
		return checkResult{
			name:   "nats",
			status: checkSKIP,
			detail: "NATS connectivity (hived not running)",
		}
	}
	defer func() {
		_ = nc.Drain()
	}()

	if !nc.IsConnected() {
		return checkResult{
			name:   "nats",
			status: checkWARN,
			detail: "NATS connection established but not in connected state",
			hint:   "Check hived logs for NATS issues",
		}
	}

	return checkResult{
		name:   "nats",
		status: checkPASS,
		detail: fmt.Sprintf("NATS connected to %s", natsURL),
	}
}

// checkVhostVsock verifies that the vhost_vsock kernel module is loaded.
func checkVhostVsock() checkResult {
	if runtime.GOOS != osLinux {
		return checkResult{
			name:   "vhost_vsock",
			status: checkSKIP,
			detail: "vhost_vsock module (Linux only, skipped on " + runtime.GOOS + ")",
		}
	}

	// Check if /dev/vhost-vsock exists.
	if _, err := os.Stat("/dev/vhost-vsock"); err == nil {
		return checkResult{
			name:   "vhost_vsock",
			status: checkPASS,
			detail: "vhost_vsock module loaded (/dev/vhost-vsock present)",
		}
	}

	// Also check /proc/modules for the module name.
	data, err := os.ReadFile("/proc/modules")
	if err == nil && strings.Contains(string(data), "vhost_vsock") {
		return checkResult{
			name:   "vhost_vsock",
			status: checkPASS,
			detail: "vhost_vsock module loaded",
		}
	}

	return checkResult{
		name:   "vhost_vsock",
		status: checkWARN,
		detail: "vhost_vsock module not loaded",
		hint:   "Load module: sudo modprobe vhost_vsock",
	}
}
