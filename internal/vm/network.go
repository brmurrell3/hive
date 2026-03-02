// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package vm

import (
	"fmt"
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
	rules = append(rules,
		"  chain egress {",
		"    type filter hook forward priority 0; policy drop;",
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
		// Allow specific destinations.
		for _, host := range p.Allowlist {
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

	rules = append(rules, "  }")

	// Ingress chain (traffic entering the VM).
	rules = append(rules,
		"  chain ingress {",
		"    type filter hook forward priority 0; policy drop;",
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

// CleanupNftables returns the nft command to remove the table for a tap device.
func CleanupNftables(tapDevice string) string {
	tableName := fmt.Sprintf("hive_%s", strings.ReplaceAll(tapDevice, "-", "_"))
	return fmt.Sprintf("nft delete table inet %s", tableName)
}
