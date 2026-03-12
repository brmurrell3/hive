// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build linux

package vm

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// tapHexSuffix matches hive TAP device names: "tap" followed by exactly 11 hex
// characters (produced by TapDeviceName). Used by CleanupStaleTapDevices to
// identify hive-managed devices without accidentally deleting user TAP devices.
var tapHexSuffix = regexp.MustCompile(`^tap[0-9a-f]{11}$`)

// tapCmdTimeout is the default timeout for individual ip/iptables commands.
const tapCmdTimeout = 15 * time.Second

// CreateTapDevice creates a TAP device with the given name using the `ip`
// command. The device name is validated against tapDevicePattern to prevent
// command injection.
func CreateTapDevice(ctx context.Context, name string) error {
	if !tapDevicePattern.MatchString(name) {
		return fmt.Errorf("invalid TAP device name %q: must match %s", name, tapDevicePattern.String())
	}

	cmdCtx, cancel := context.WithTimeout(ctx, tapCmdTimeout)
	defer cancel()

	out, err := exec.CommandContext(cmdCtx, "ip", "tuntap", "add", "dev", name, "mode", "tap").CombinedOutput()
	if err != nil {
		return fmt.Errorf("creating TAP device %s: %w (output: %s)", name, err, strings.TrimSpace(string(out)))
	}

	slog.Info("TAP device created", "device", name)
	return nil
}

// ConfigureTapDevice assigns an IP address to the TAP device and brings it up.
// The ipAddr should be in CIDR notation (e.g., "172.16.0.1/30").
func ConfigureTapDevice(ctx context.Context, name, ipAddr string) error {
	if !tapDevicePattern.MatchString(name) {
		return fmt.Errorf("invalid TAP device name %q: must match %s", name, tapDevicePattern.String())
	}

	// Assign the IP address.
	addrCtx, addrCancel := context.WithTimeout(ctx, tapCmdTimeout)
	defer addrCancel()

	out, err := exec.CommandContext(addrCtx, "ip", "addr", "add", ipAddr, "dev", name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("assigning IP %s to TAP device %s: %w (output: %s)", ipAddr, name, err, strings.TrimSpace(string(out)))
	}

	// Bring the device up.
	upCtx, upCancel := context.WithTimeout(ctx, tapCmdTimeout)
	defer upCancel()

	out, err = exec.CommandContext(upCtx, "ip", "link", "set", name, "up").CombinedOutput()
	if err != nil {
		return fmt.Errorf("bringing up TAP device %s: %w (output: %s)", name, err, strings.TrimSpace(string(out)))
	}

	slog.Info("TAP device configured", "device", name, "ip", ipAddr)
	return nil
}

// SetupNAT enables IP forwarding and configures iptables masquerade NAT for
// traffic originating from the TAP device's subnet. This allows guest VMs to
// reach external networks through the host.
func SetupNAT(ctx context.Context, tapName string) error {
	if !tapDevicePattern.MatchString(tapName) {
		return fmt.Errorf("invalid TAP device name %q: must match %s", tapName, tapDevicePattern.String())
	}

	// Enable IP forwarding via procfs (idempotent).
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644); err != nil {
		return fmt.Errorf("enabling IP forwarding: %w", err)
	}

	// Add iptables masquerade rule for outbound traffic from the TAP device.
	// Use -C (check) first to avoid duplicate rules.
	checkCtx, checkCancel := context.WithTimeout(ctx, tapCmdTimeout)
	defer checkCancel()

	checkErr := exec.CommandContext(checkCtx, "iptables", "-t", "nat",
		"-C", "POSTROUTING", "-o", tapName, "-j", "MASQUERADE").Run()
	if checkErr != nil {
		// Rule does not exist; add it.
		addCtx, addCancel := context.WithTimeout(ctx, tapCmdTimeout)
		defer addCancel()

		out, err := exec.CommandContext(addCtx, "iptables", "-t", "nat",
			"-A", "POSTROUTING", "-o", tapName, "-j", "MASQUERADE").CombinedOutput()
		if err != nil {
			return fmt.Errorf("adding iptables masquerade rule for %s: %w (output: %s)", tapName, err, strings.TrimSpace(string(out)))
		}
	}

	// Add FORWARD accept rule for traffic from the TAP device.
	fwdCheckCtx, fwdCheckCancel := context.WithTimeout(ctx, tapCmdTimeout)
	defer fwdCheckCancel()

	fwdCheckErr := exec.CommandContext(fwdCheckCtx, "iptables",
		"-C", "FORWARD", "-i", tapName, "-j", "ACCEPT").Run()
	if fwdCheckErr != nil {
		fwdAddCtx, fwdAddCancel := context.WithTimeout(ctx, tapCmdTimeout)
		defer fwdAddCancel()

		out, err := exec.CommandContext(fwdAddCtx, "iptables",
			"-A", "FORWARD", "-i", tapName, "-j", "ACCEPT").CombinedOutput()
		if err != nil {
			return fmt.Errorf("adding iptables FORWARD rule for %s: %w (output: %s)", tapName, err, strings.TrimSpace(string(out)))
		}
	}

	// Add FORWARD accept rule for established/related return traffic to the TAP device.
	retCheckCtx, retCheckCancel := context.WithTimeout(ctx, tapCmdTimeout)
	defer retCheckCancel()

	retCheckErr := exec.CommandContext(retCheckCtx, "iptables",
		"-C", "FORWARD", "-o", tapName, "-m", "state", "--state", "ESTABLISHED,RELATED", "-j", "ACCEPT").Run()
	if retCheckErr != nil {
		retAddCtx, retAddCancel := context.WithTimeout(ctx, tapCmdTimeout)
		defer retAddCancel()

		out, err := exec.CommandContext(retAddCtx, "iptables",
			"-A", "FORWARD", "-o", tapName, "-m", "state", "--state", "ESTABLISHED,RELATED", "-j", "ACCEPT").CombinedOutput()
		if err != nil {
			return fmt.Errorf("adding iptables return traffic rule for %s: %w (output: %s)", tapName, err, strings.TrimSpace(string(out)))
		}
	}

	slog.Info("NAT configured for TAP device", "device", tapName)
	return nil
}

// DeleteTapDevice removes a TAP device using the `ip` command.
// Errors are returned if the device name is invalid; deletion failures
// (e.g., device does not exist) are logged but returned as errors so the
// caller can decide whether to treat them as fatal.
func DeleteTapDevice(ctx context.Context, name string) error {
	if !tapDevicePattern.MatchString(name) {
		return fmt.Errorf("invalid TAP device name %q: must match %s", name, tapDevicePattern.String())
	}

	cmdCtx, cancel := context.WithTimeout(ctx, tapCmdTimeout)
	defer cancel()

	out, err := exec.CommandContext(cmdCtx, "ip", "link", "delete", name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("deleting TAP device %s: %w (output: %s)", name, err, strings.TrimSpace(string(out)))
	}

	slog.Info("TAP device deleted", "device", name)
	return nil
}

// CleanupStaleTapDevices finds and removes any TAP devices whose names match
// hive's naming pattern (tap + 11 hex chars). This is used during startup
// reconciliation to clean up TAP devices left behind by a previous crash.
func CleanupStaleTapDevices(ctx context.Context) error {
	listCtx, listCancel := context.WithTimeout(ctx, tapCmdTimeout)
	defer listCancel()

	out, err := exec.CommandContext(listCtx, "ip", "-o", "link", "show", "type", "tun").CombinedOutput()
	if err != nil {
		return fmt.Errorf("listing TUN/TAP devices: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}

	// Parse output lines to extract device names. Each line from `ip -o link show`
	// has the format: "<index>: <device>[@<parent>]: <flags> ..."
	var cleaned int
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Extract device name: split on ":" and take the second field.
		fields := strings.SplitN(line, ":", 3)
		if len(fields) < 2 {
			continue
		}
		devName := strings.TrimSpace(fields[1])
		// Strip any @parent suffix (e.g., "tap0@if2").
		if idx := strings.Index(devName, "@"); idx >= 0 {
			devName = devName[:idx]
		}

		if !tapHexSuffix.MatchString(devName) {
			continue
		}

		slog.Info("cleaning up stale TAP device", "device", devName)
		if delErr := DeleteTapDevice(ctx, devName); delErr != nil {
			slog.Warn("failed to clean up stale TAP device", "device", devName, "error", delErr)
		} else {
			cleaned++
		}
	}

	if cleaned > 0 {
		slog.Info("stale TAP device cleanup complete", "cleaned", cleaned)
	}
	return nil
}

// TapSubnet computes the host-side and guest-side IP addresses for a given CID.
// Each agent gets a /30 subnet from the 172.16.0.0/16 space.
//
// The CID (starting at 3) is used to derive the subnet offset:
//   - offset = CID - 3
//   - high byte = offset / 64
//   - low byte = (offset % 64) * 4
//   - Host IP: 172.16.{high}.{low+1}/30
//   - Guest IP: 172.16.{high}.{low+2}/30
//
// Returns the host IP in CIDR notation and the guest IP as a plain address.
func TapSubnet(cid uint32) (hostCIDR string, guestIP string, err error) {
	if cid < 3 {
		return "", "", fmt.Errorf("CID %d is reserved (must be >= 3)", cid)
	}

	offset := cid - 3

	// Each /30 uses 4 addresses. With a /16 we have 65536 addresses,
	// yielding 16384 /30 subnets maximum.
	if offset >= 16384 {
		return "", "", fmt.Errorf("CID %d exceeds maximum addressable subnets (max offset 16383)", cid)
	}

	high := offset / 64
	low := (offset % 64) * 4

	hostCIDR = fmt.Sprintf("172.16.%d.%d/30", high, low+1)
	guestIP = fmt.Sprintf("172.16.%d.%d", high, low+2)
	return hostCIDR, guestIP, nil
}
