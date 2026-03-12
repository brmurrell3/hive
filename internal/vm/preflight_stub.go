// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build !linux

package vm

import (
	"fmt"
	"runtime"
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

// CheckCapabilities returns platform-appropriate warnings on non-Linux systems.
// Firecracker VMs require Linux with KVM, so all checks return warnings
// indicating that the platform is unsupported for VM workloads.
func CheckCapabilities() []PreflightResult {
	msg := fmt.Sprintf("running on %s/%s: Firecracker VMs require Linux with KVM; VM workloads will use the process backend", runtime.GOOS, runtime.GOARCH)

	return []PreflightResult{
		{
			Name:    "platform",
			Status:  PreflightWarn,
			Message: msg,
		},
		{
			Name:    "kvm_device",
			Status:  PreflightWarn,
			Message: "/dev/kvm is not available on " + runtime.GOOS,
		},
		{
			Name:    "cap_net_admin",
			Status:  PreflightWarn,
			Message: "CAP_NET_ADMIN check is not applicable on " + runtime.GOOS,
		},
		{
			Name:    "cap_sys_admin",
			Status:  PreflightWarn,
			Message: "CAP_SYS_ADMIN check is not applicable on " + runtime.GOOS,
		},
		{
			Name:    "nft_binary",
			Status:  PreflightWarn,
			Message: "nft binary check is not applicable on " + runtime.GOOS,
		},
		{
			Name:    "ip_binary",
			Status:  PreflightWarn,
			Message: "ip binary check is not applicable on " + runtime.GOOS,
		},
		{
			Name:    "vhost_vsock",
			Status:  PreflightWarn,
			Message: "vhost_vsock module check is not applicable on " + runtime.GOOS,
		},
	}
}
