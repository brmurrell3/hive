// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package vm

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"strings"
)

const egressFull = "full"

// NetworkPolicy describes the network restrictions for an agent.
type NetworkPolicy struct {
	TapDevice string   // e.g., "tap0"
	Egress    string   // "none", "restricted", "full"
	Allowlist []string // hostnames/IPs allowed when egress is "restricted"
	Ingress   string   // "none", "restricted", "full"
}

// GenerateNftables produces nftables rules for the given policy.
// Rules are returned as a string suitable for `nft -f`.
func GenerateNftables(p NetworkPolicy) string {
	if p.TapDevice == "" {
		return ""
	}

	var rules []string
	tableName := fmt.Sprintf("hive_%s", strings.ReplaceAll(p.TapDevice, "-", "_"))

	rules = append(rules,
		fmt.Sprintf("table inet %s {", tableName),
	)

	// Egress chain (traffic leaving the VM).
	// Uses policy accept so that forwarded traffic for OTHER interfaces is
	// not affected. An explicit drop rule scoped to this tap device is
	// appended at the end to deny unmatched egress from this VM.
	rules = append(rules,
		"  chain egress {",
		"    type filter hook forward priority 0; policy accept;",
	)

	switch p.Egress {
	case egressFull:
		rules = append(rules,
			fmt.Sprintf("    iifname %q accept", p.TapDevice),
		)
	case "restricted":
		// Allow DNS always.
		rules = append(rules,
			fmt.Sprintf("    iifname %q udp dport 53 accept", p.TapDevice),
			fmt.Sprintf("    iifname %q tcp dport 53 accept", p.TapDevice),
		)
		// Allow NATS (4222).
		rules = append(rules,
			fmt.Sprintf("    iifname %q tcp dport 4222 accept", p.TapDevice),
		)
		// Resolve hostnames to IPs before generating rules.
		resolvedHosts := resolveAllowlistHosts(p.Allowlist)
		// Allow specific destinations.
		for _, host := range resolvedHosts {
			if err := validateAllowlistHost(host); err != nil {
				continue // skip invalid entries silently (logged at call site)
			}
			rules = append(rules,
				fmt.Sprintf("    iifname %q ip daddr %s accept", p.TapDevice, host),
			)
		}
		// Allow established/related connections.
		rules = append(rules,
			fmt.Sprintf("    iifname %q ct state established,related accept", p.TapDevice),
		)
	case "none":
		// Only allow NATS so the agent can communicate with the control plane.
		rules = append(rules,
			fmt.Sprintf("    iifname %q tcp dport 4222 accept", p.TapDevice),
			fmt.Sprintf("    iifname %q ct state established,related accept", p.TapDevice),
		)
	}

	// Drop remaining traffic from this tap device only; traffic from
	// other interfaces passes through unaffected.
	rules = append(rules,
		fmt.Sprintf("    iifname %q drop", p.TapDevice),
	)

	rules = append(rules, "  }")

	// Ingress chain (traffic entering the VM).
	// Uses policy accept so that forwarded traffic for OTHER interfaces is
	// not affected. An explicit drop rule scoped to this tap device is
	// appended at the end to deny unmatched ingress to this VM.
	rules = append(rules,
		"  chain ingress {",
		"    type filter hook forward priority 0; policy accept;",
	)

	switch p.Ingress {
	case egressFull:
		rules = append(rules,
			fmt.Sprintf("    oifname %q accept", p.TapDevice),
		)
	case "restricted", "":
		// Only allow established/related connections (replies to outbound).
		rules = append(rules,
			fmt.Sprintf("    oifname %q ct state established,related accept", p.TapDevice),
		)
	case "none":
		rules = append(rules,
			fmt.Sprintf("    oifname %q ct state established,related accept", p.TapDevice),
		)
	}

	// Drop remaining traffic destined for this tap device only; traffic
	// for other interfaces passes through unaffected.
	rules = append(rules,
		fmt.Sprintf("    oifname %q drop", p.TapDevice),
	)

	rules = append(rules, "  }")
	rules = append(rules, "}")

	return strings.Join(rules, "\n")
}

// validateAllowlistHost checks that a host entry is a valid IP or CIDR,
// preventing injection of arbitrary strings into nftables rules.
func validateAllowlistHost(host string) error {
	if net.ParseIP(host) != nil {
		return nil
	}
	if _, _, err := net.ParseCIDR(host); err == nil {
		return nil
	}
	return fmt.Errorf("invalid allowlist host %q: must be IP or CIDR", host)
}

// TapDeviceName returns a deterministic tap device name for an agent that
// fits within the IFNAMSIZ limit (15 chars including NUL terminator, so 14
// usable chars). It uses a SHA-256 hash of the agentID to produce a unique,
// truncated name: "tap" + 11 hex chars = 14 chars.
func TapDeviceName(agentID string) string {
	h := sha256.Sum256([]byte(agentID))
	// 6 bytes = 12 hex chars; we take 11 to fit "tap" + 11 = 14 chars
	return "tap" + hex.EncodeToString(h[:6])[:11]
}

// resolveAllowlistHosts resolves hostname entries in the allowlist to IP
// addresses using DNS lookup. IPs and CIDRs are passed through unchanged.
// Unresolvable hostnames are logged and skipped.
func resolveAllowlistHosts(hosts []string) []string {
	var resolved []string
	for _, host := range hosts {
		// If it's already a valid IP or CIDR, keep it as-is.
		if net.ParseIP(host) != nil {
			resolved = append(resolved, host)
			continue
		}
		if _, _, err := net.ParseCIDR(host); err == nil {
			resolved = append(resolved, host)
			continue
		}
		// It's a hostname -- resolve to IPs.
		ips, err := net.LookupHost(host)
		if err != nil {
			slog.Warn("failed to resolve allowlist hostname, skipping",
				"hostname", host,
				"error", err,
			)
			continue
		}
		for _, ip := range ips {
			resolved = append(resolved, ip)
		}
	}
	return resolved
}

// CleanupNftables returns the command name and arguments to remove the
// nftables table for a tap device. The caller can pass these directly to
// exec.CommandContext(ctx, cmd, args...) without string splitting.
func CleanupNftables(tapDevice string) (string, []string) {
	tableName := fmt.Sprintf("hive_%s", strings.ReplaceAll(tapDevice, "-", "_"))
	return "nft", []string{"delete", "table", "inet", tableName}
}
