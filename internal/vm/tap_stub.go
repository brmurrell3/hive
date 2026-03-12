// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build !linux

package vm

import (
	"context"
	"fmt"
)

// CreateTapDevice is a stub for non-Linux platforms. TAP devices require the
// Linux TUN/TAP kernel module.
func CreateTapDevice(_ context.Context, name string) error {
	return fmt.Errorf("TAP device creation is not supported on this platform (requires Linux): %s", name)
}

// ConfigureTapDevice is a stub for non-Linux platforms.
func ConfigureTapDevice(_ context.Context, name, ipAddr string) error {
	return fmt.Errorf("TAP device configuration is not supported on this platform (requires Linux): %s", name)
}

// SetupNAT is a stub for non-Linux platforms.
func SetupNAT(_ context.Context, tapName string) error {
	return fmt.Errorf("NAT setup is not supported on this platform (requires Linux): %s", tapName)
}

// DeleteTapDevice is a stub for non-Linux platforms.
func DeleteTapDevice(_ context.Context, name string) error {
	return fmt.Errorf("TAP device deletion is not supported on this platform (requires Linux): %s", name)
}

// CleanupStaleTapDevices is a stub for non-Linux platforms.
func CleanupStaleTapDevices(_ context.Context) error {
	return fmt.Errorf("TAP device cleanup is not supported on this platform (requires Linux)")
}

// TapSubnet computes the host-side and guest-side IP addresses for a given CID.
// This function is platform-independent and works on all platforms.
func TapSubnet(cid uint32) (hostCIDR string, guestIP string, err error) {
	if cid < 3 {
		return "", "", fmt.Errorf("CID %d is reserved (must be >= 3)", cid)
	}

	offset := cid - 3

	if offset >= 16384 {
		return "", "", fmt.Errorf("CID %d exceeds maximum addressable subnets (max offset 16383)", cid)
	}

	high := offset / 64
	low := (offset % 64) * 4

	hostCIDR = fmt.Sprintf("172.16.%d.%d/30", high, low+1)
	guestIP = fmt.Sprintf("172.16.%d.%d", high, low+2)
	return hostCIDR, guestIP, nil
}
