// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package vm

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"regexp"
	"strings"
)

// tapDevicePattern validates tap device names: alphanumeric, underscore, and
// hyphen only, 1-15 characters (IFNAMSIZ limit minus NUL terminator).
var tapDevicePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,15}$`)

const egressFull = "full"
const ingressFull = "full"

// NetworkPolicy describes the network restrictions for an agent.
type NetworkPolicy struct {
	TapDevice string   // e.g., "tap0"
	Egress    string   // "none", "restricted", "full"
	Allowlist []string // hostnames/IPs allowed when egress is "restricted"
	Ingress   string   // "none", "restricted", "full"
	GatewayIP string   // host-side IP of the TAP interface (used to scope DNS rules)
}

// GenerateNftables produces nftables rules for the given policy.
// Rules are returned as a string suitable for `nft -f`.
// Returns an error if allowlist hostname resolution fails.
func GenerateNftables(p NetworkPolicy) (string, error) {
	if p.TapDevice == "" {
		return "", nil
	}

	// NET-H2: Validate TapDevice to prevent nftables injection.
	if !tapDevicePattern.MatchString(p.TapDevice) {
		return "", fmt.Errorf("invalid TapDevice %q: must match %s", p.TapDevice, tapDevicePattern.String())
	}

	// NET-C2: Validate GatewayIP before nftables rule generation.
	if p.GatewayIP != "" && net.ParseIP(p.GatewayIP) == nil {
		return "", fmt.Errorf("invalid GatewayIP %q", p.GatewayIP)
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
		// Allow DNS scoped to the gateway IP (host-side of TAP) to prevent
		// DNS traffic from reaching arbitrary destinations. If no gateway IP
		// is configured, fall back to unscoped rules for backward compatibility.
		if p.GatewayIP != "" {
			directive := nftAddrDirective(p.GatewayIP)
			rules = append(rules,
				fmt.Sprintf("    iifname %q %s %s udp dport 53 accept", p.TapDevice, directive, p.GatewayIP),
				fmt.Sprintf("    iifname %q %s %s tcp dport 53 accept", p.TapDevice, directive, p.GatewayIP),
			)
		} else {
			rules = append(rules,
				fmt.Sprintf("    iifname %q udp dport 53 accept", p.TapDevice),
				fmt.Sprintf("    iifname %q tcp dport 53 accept", p.TapDevice),
			)
		}
		// NET-C1: Allow NATS (4222) scoped to GatewayIP when available.
		if p.GatewayIP != "" {
			directive := nftAddrDirective(p.GatewayIP)
			rules = append(rules,
				fmt.Sprintf("    iifname %q %s %s tcp dport 4222 accept", p.TapDevice, directive, p.GatewayIP),
			)
		} else {
			rules = append(rules,
				fmt.Sprintf("    iifname %q tcp dport 4222 accept", p.TapDevice),
			)
		}
		// Resolve hostnames to IPs before generating rules.
		resolvedHosts, resolveErr := resolveAllowlistHosts(p.Allowlist)
		if resolveErr != nil {
			return "", resolveErr
		}
		// Allow specific destinations.
		for _, host := range resolvedHosts {
			if err := validateAllowlistHost(host); err != nil {
				continue // skip invalid entries silently (logged at call site)
			}
			rules = append(rules,
				fmt.Sprintf("    iifname %q %s %s accept", p.TapDevice, nftAddrDirective(host), host),
			)
		}
		// Allow established/related connections.
		rules = append(rules,
			fmt.Sprintf("    iifname %q ct state established,related accept", p.TapDevice),
		)
	case "none":
		// NET-C1: Only allow NATS so the agent can communicate with the control plane,
		// scoped to GatewayIP when available.
		if p.GatewayIP != "" {
			directive := nftAddrDirective(p.GatewayIP)
			rules = append(rules,
				fmt.Sprintf("    iifname %q %s %s tcp dport 4222 accept", p.TapDevice, directive, p.GatewayIP),
			)
		} else {
			rules = append(rules,
				fmt.Sprintf("    iifname %q tcp dport 4222 accept", p.TapDevice),
			)
		}
		rules = append(rules,
			fmt.Sprintf("    iifname %q ct state established,related accept", p.TapDevice),
		)
	default:
		// NET-H1: Reject unrecognized egress values.
		return "", fmt.Errorf("unrecognized egress policy %q: must be \"full\", \"restricted\", or \"none\"", p.Egress)
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
	case ingressFull:
		rules = append(rules,
			fmt.Sprintf("    oifname %q accept", p.TapDevice),
		)
	case "restricted", "":
		// Only allow established/related connections (replies to outbound).
		rules = append(rules,
			fmt.Sprintf("    oifname %q ct state established,related accept", p.TapDevice),
		)
	case "none":
		// For true "none" ingress, only allow established/related replies
		// for the NATS port — not for all traffic.
		rules = append(rules,
			fmt.Sprintf("    oifname %q tcp sport 4222 ct state established,related accept", p.TapDevice),
		)
	default:
		// NET-H1: Reject unrecognized ingress values.
		return "", fmt.Errorf("unrecognized ingress policy %q: must be \"full\", \"restricted\", \"\", or \"none\"", p.Ingress)
	}

	// Drop remaining traffic destined for this tap device only; traffic
	// for other interfaces passes through unaffected.
	rules = append(rules,
		fmt.Sprintf("    oifname %q drop", p.TapDevice),
	)

	rules = append(rules, "  }")
	rules = append(rules, "}")

	return strings.Join(rules, "\n"), nil
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

// nftAddrDirective returns the correct nftables address family directive for
// the given IP or CIDR. IPv6 addresses (containing ':') use "ip6 daddr",
// while IPv4 addresses use "ip daddr".
func nftAddrDirective(addr string) string {
	if strings.Contains(addr, ":") {
		return "ip6 daddr"
	}
	return "ip daddr"
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
// Returns an error if any hostname cannot be resolved (NET-H3: fail loudly
// instead of silently skipping DNS failures).
func resolveAllowlistHosts(hosts []string) ([]string, error) {
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
			return nil, fmt.Errorf("resolving allowlist hostname %q: %w", host, err)
		}
		for _, ip := range ips {
			resolved = append(resolved, ip)
		}
	}
	return resolved, nil
}

// CleanupNftables returns the command name and arguments to remove the
// nftables table for a tap device. The caller can pass these directly to
// exec.CommandContext(ctx, cmd, args...) without string splitting.
func CleanupNftables(tapDevice string) (string, []string) {
	tableName := fmt.Sprintf("hive_%s", strings.ReplaceAll(tapDevice, "-", "_"))
	return "nft", []string{"delete", "table", "inet", tableName}
}
