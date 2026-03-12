// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build linux

package vm

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// PreflightStatus represents the result status of a pre-flight check.
type PreflightStatus string

const (
	// PreflightPass indicates the check passed successfully.
	PreflightPass PreflightStatus = "pass"
	// PreflightFail indicates the check failed (critical).
	PreflightFail PreflightStatus = "fail"
	// PreflightWarn indicates the check produced a warning (non-critical).
	PreflightWarn PreflightStatus = "warn"
)

// PreflightResult holds the outcome of a single pre-flight check.
type PreflightResult struct {
	Name    string          // Human-readable name of the check.
	Status  PreflightStatus // pass, fail, or warn.
	Message string          // Description of the result.
}

// CheckCapabilities runs all pre-flight checks for VM/network requirements
// on Linux and returns a list of results. Each check is independent; all
// checks run regardless of earlier failures.
func CheckCapabilities() []PreflightResult {
	var results []PreflightResult

	results = append(results, checkKVMDevice())
	results = append(results, checkCapNetAdmin())
	results = append(results, checkCapSysAdmin())
	results = append(results, checkNftBinary())
	results = append(results, checkIPBinary())
	results = append(results, checkVhostVsock())

	return results
}

// checkKVMDevice verifies that /dev/kvm exists and is accessible (read/write).
func checkKVMDevice() PreflightResult {
	info, err := os.Stat("/dev/kvm")
	if err != nil {
		return PreflightResult{
			Name:    "kvm_device",
			Status:  PreflightFail,
			Message: fmt.Sprintf("/dev/kvm is not available: %v", err),
		}
	}

	// Check read/write access by attempting to open it.
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return PreflightResult{
			Name:    "kvm_device",
			Status:  PreflightFail,
			Message: fmt.Sprintf("/dev/kvm exists but is not accessible (mode %v): %v", info.Mode(), err),
		}
	}
	f.Close()

	return PreflightResult{
		Name:    "kvm_device",
		Status:  PreflightPass,
		Message: "/dev/kvm is available and accessible",
	}
}

// checkCapNetAdmin checks for CAP_NET_ADMIN capability by attempting a
// harmless network namespace operation. This capability is required for
// creating TAP devices and applying nftables rules.
func checkCapNetAdmin() PreflightResult {
	// A reliable heuristic: try to list nftables tables. If we have
	// CAP_NET_ADMIN, this succeeds (even with no tables). If not, it
	// fails with a permission error.
	nftPath, err := exec.LookPath("nft")
	if err != nil {
		// If nft is not installed, we cannot check CAP_NET_ADMIN this way.
		// Return warn since the nft binary check will catch the missing binary.
		return PreflightResult{
			Name:    "cap_net_admin",
			Status:  PreflightWarn,
			Message: "cannot verify CAP_NET_ADMIN: nft binary not found in PATH",
		}
	}

	out, err := exec.Command(nftPath, "list", "tables").CombinedOutput()
	if err != nil {
		outStr := strings.TrimSpace(string(out))
		return PreflightResult{
			Name:    "cap_net_admin",
			Status:  PreflightFail,
			Message: fmt.Sprintf("CAP_NET_ADMIN appears missing (nft list tables failed: %s)", outStr),
		}
	}

	return PreflightResult{
		Name:    "cap_net_admin",
		Status:  PreflightPass,
		Message: "CAP_NET_ADMIN is available (nft list tables succeeded)",
	}
}

// checkCapSysAdmin checks for CAP_SYS_ADMIN capability by reading
// /proc/self/status for the CapEff bitmask. CAP_SYS_ADMIN is bit 21.
// This capability is required for KVM device access in some configurations.
func checkCapSysAdmin() PreflightResult {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return PreflightResult{
			Name:    "cap_sys_admin",
			Status:  PreflightWarn,
			Message: fmt.Sprintf("cannot read /proc/self/status to check CAP_SYS_ADMIN: %v", err),
		}
	}

	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "CapEff:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			break
		}
		hexVal := fields[1]
		// Parse the hex capability mask.
		var capEff uint64
		_, err := fmt.Sscanf(hexVal, "%x", &capEff)
		if err != nil {
			return PreflightResult{
				Name:    "cap_sys_admin",
				Status:  PreflightWarn,
				Message: fmt.Sprintf("cannot parse CapEff value %q: %v", hexVal, err),
			}
		}
		// CAP_SYS_ADMIN is bit 21.
		if capEff&(1<<21) != 0 {
			return PreflightResult{
				Name:    "cap_sys_admin",
				Status:  PreflightPass,
				Message: "CAP_SYS_ADMIN is present in effective capabilities",
			}
		}
		return PreflightResult{
			Name:    "cap_sys_admin",
			Status:  PreflightWarn,
			Message: "CAP_SYS_ADMIN is not present in effective capabilities; KVM may still work if /dev/kvm permissions allow it",
		}
	}

	return PreflightResult{
		Name:    "cap_sys_admin",
		Status:  PreflightWarn,
		Message: "CapEff line not found in /proc/self/status",
	}
}

// checkNftBinary verifies that the nft binary is available in PATH.
func checkNftBinary() PreflightResult {
	path, err := exec.LookPath("nft")
	if err != nil {
		return PreflightResult{
			Name:    "nft_binary",
			Status:  PreflightFail,
			Message: "nft binary not found in PATH (required for network policy enforcement)",
		}
	}

	return PreflightResult{
		Name:    "nft_binary",
		Status:  PreflightPass,
		Message: fmt.Sprintf("nft binary found at %s", path),
	}
}

// checkIPBinary verifies that the ip binary is available in PATH.
func checkIPBinary() PreflightResult {
	path, err := exec.LookPath("ip")
	if err != nil {
		return PreflightResult{
			Name:    "ip_binary",
			Status:  PreflightFail,
			Message: "ip binary not found in PATH (required for TAP device creation)",
		}
	}

	return PreflightResult{
		Name:    "ip_binary",
		Status:  PreflightPass,
		Message: fmt.Sprintf("ip binary found at %s", path),
	}
}

// checkVhostVsock checks whether the vhost_vsock kernel module is loaded
// by looking for it in /sys/module/vhost_vsock or /proc/modules.
func checkVhostVsock() PreflightResult {
	// First try /sys/module/vhost_vsock which is the most reliable check.
	if _, err := os.Stat("/sys/module/vhost_vsock"); err == nil {
		return PreflightResult{
			Name:    "vhost_vsock",
			Status:  PreflightPass,
			Message: "vhost_vsock kernel module is loaded (/sys/module/vhost_vsock exists)",
		}
	}

	// Fallback: check /proc/modules for the module name.
	data, err := os.ReadFile("/proc/modules")
	if err != nil {
		return PreflightResult{
			Name:    "vhost_vsock",
			Status:  PreflightWarn,
			Message: fmt.Sprintf("cannot verify vhost_vsock module: unable to read /sys/module or /proc/modules: %v", err),
		}
	}

	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "vhost_vsock" {
			return PreflightResult{
				Name:    "vhost_vsock",
				Status:  PreflightPass,
				Message: "vhost_vsock kernel module is loaded (found in /proc/modules)",
			}
		}
	}

	return PreflightResult{
		Name:    "vhost_vsock",
		Status:  PreflightWarn,
		Message: "vhost_vsock kernel module does not appear to be loaded; vsock communication may not work",
	}
}
