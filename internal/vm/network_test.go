// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build unit

package vm

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// GenerateNftables: egress "none" — NATS accept + drop all
// ---------------------------------------------------------------------------

func TestGenerateNftables_EgressNone(t *testing.T) {
	rules := GenerateNftables(NetworkPolicy{
		TapDevice: "tap0",
		Egress:    "none",
	})

	// NATS port 4222 must be accepted.
	if !strings.Contains(rules, "tcp dport 4222 accept") {
		t.Error("egress none: expected NATS port 4222 accept rule")
	}
	// Established/related must be accepted.
	if !strings.Contains(rules, "ct state established,related accept") {
		t.Error("egress none: expected established/related accept rule")
	}
	// A scoped drop rule for the tap device must be present.
	if !strings.Contains(rules, `iifname "tap0" drop`) {
		t.Error("egress none: expected scoped drop rule for tap0")
	}
}

// ---------------------------------------------------------------------------
// GenerateNftables: egress "restricted" with IP allowlist
// ---------------------------------------------------------------------------

func TestGenerateNftables_EgressRestrictedAllowlist(t *testing.T) {
	rules := GenerateNftables(NetworkPolicy{
		TapDevice: "tap0",
		Egress:    "restricted",
		Allowlist: []string{"10.0.0.1", "192.168.1.0/24"},
	})

	if !strings.Contains(rules, "10.0.0.1") {
		t.Error("restricted: expected allowlist IP 10.0.0.1 in rules")
	}
	if !strings.Contains(rules, "192.168.1.0/24") {
		t.Error("restricted: expected allowlist CIDR 192.168.1.0/24 in rules")
	}
	// Both should use ip daddr (IPv4).
	if strings.Count(rules, "ip daddr") < 2 {
		t.Error("restricted: expected at least 2 'ip daddr' directives for IPv4 allowlist entries")
	}
	// NATS port must still be present.
	if !strings.Contains(rules, "tcp dport 4222 accept") {
		t.Error("restricted: expected NATS port 4222 accept rule")
	}
}

// ---------------------------------------------------------------------------
// GenerateNftables: egress "full" — accept rule present
// ---------------------------------------------------------------------------

func TestGenerateNftables_EgressFull(t *testing.T) {
	rules := GenerateNftables(NetworkPolicy{
		TapDevice: "tap0",
		Egress:    "full",
	})

	if !strings.Contains(rules, `iifname "tap0" accept`) {
		t.Error("egress full: expected unconditional accept rule for tap0")
	}
}

// ---------------------------------------------------------------------------
// GenerateNftables: IPv6 allowlist entries use ip6 daddr
// ---------------------------------------------------------------------------

func TestGenerateNftables_IPv6Allowlist(t *testing.T) {
	rules := GenerateNftables(NetworkPolicy{
		TapDevice: "tap0",
		Egress:    "restricted",
		Allowlist: []string{"2001:db8::1", "fd00::/64"},
	})

	if !strings.Contains(rules, "ip6 daddr") {
		t.Error("IPv6 allowlist: expected 'ip6 daddr' directive")
	}
	if !strings.Contains(rules, "2001:db8::1") {
		t.Error("IPv6 allowlist: expected address 2001:db8::1 in rules")
	}
	if !strings.Contains(rules, "fd00::/64") {
		t.Error("IPv6 allowlist: expected CIDR fd00::/64 in rules")
	}
}

// ---------------------------------------------------------------------------
// GenerateNftables: GatewayIP set — DNS rules scoped to gateway
// ---------------------------------------------------------------------------

func TestGenerateNftables_GatewayIP(t *testing.T) {
	rules := GenerateNftables(NetworkPolicy{
		TapDevice: "tap0",
		Egress:    "restricted",
		GatewayIP: "10.0.0.1",
		Allowlist: []string{"1.2.3.4"},
	})

	// DNS rules should include the gateway IP.
	if !strings.Contains(rules, "10.0.0.1 udp dport 53 accept") {
		t.Error("GatewayIP: expected DNS UDP rule scoped to gateway 10.0.0.1")
	}
	if !strings.Contains(rules, "10.0.0.1 tcp dport 53 accept") {
		t.Error("GatewayIP: expected DNS TCP rule scoped to gateway 10.0.0.1")
	}

	// Verify the DNS rules use ip daddr for the gateway.
	if !strings.Contains(rules, "ip daddr 10.0.0.1 udp dport 53") {
		t.Error("GatewayIP: expected 'ip daddr' directive for gateway DNS rules")
	}
}

// ---------------------------------------------------------------------------
// GenerateNftables: empty TapDevice — returns empty string
// ---------------------------------------------------------------------------

func TestGenerateNftables_EmptyTapDevice(t *testing.T) {
	rules := GenerateNftables(NetworkPolicy{
		TapDevice: "",
		Egress:    "none",
	})
	if rules != "" {
		t.Errorf("expected empty string for empty TapDevice, got %q", rules)
	}
}

// ---------------------------------------------------------------------------
// TapDeviceName: determinism — same agentID produces same device name
// ---------------------------------------------------------------------------

func TestTapDeviceName_Deterministic(t *testing.T) {
	name1 := TapDeviceName("agent-alpha")
	name2 := TapDeviceName("agent-alpha")

	if name1 != name2 {
		t.Errorf("TapDeviceName not deterministic: %q != %q", name1, name2)
	}

	// Different agent IDs should produce different names.
	name3 := TapDeviceName("agent-beta")
	if name1 == name3 {
		t.Errorf("different agentIDs produced same TapDeviceName: %q", name1)
	}
}

// ---------------------------------------------------------------------------
// TapDeviceName: length <= 15 chars (IFNAMSIZ - 1 for NUL)
// ---------------------------------------------------------------------------

func TestTapDeviceName_Length(t *testing.T) {
	testIDs := []string{
		"short",
		"a-very-long-agent-identifier-that-should-still-produce-a-short-name",
		"agent-123",
		"",
	}

	for _, id := range testIDs {
		name := TapDeviceName(id)
		// IFNAMSIZ is 16 (including NUL), so usable length is 15.
		// The code produces "tap" + 11 hex = 14 chars, well within limit.
		if len(name) > 15 {
			t.Errorf("TapDeviceName(%q) = %q (len %d), exceeds 15 chars", id, name, len(name))
		}
		if !strings.HasPrefix(name, "tap") {
			t.Errorf("TapDeviceName(%q) = %q, expected 'tap' prefix", id, name)
		}
	}
}

// ---------------------------------------------------------------------------
// validateAllowlistHost: accepts valid IPs and CIDRs, rejects injection
// ---------------------------------------------------------------------------

func TestValidateAllowlistHost(t *testing.T) {
	valid := []string{
		"10.0.0.1",
		"192.168.1.0/24",
		"::1",
		"2001:db8::1",
		"fe80::/10",
		"0.0.0.0",
		"255.255.255.255",
	}
	for _, h := range valid {
		if err := validateAllowlistHost(h); err != nil {
			t.Errorf("validateAllowlistHost(%q) returned error: %v", h, err)
		}
	}

	invalid := []string{
		"example.com",
		"; drop table --",
		"10.0.0.1; nft delete table",
		"../../etc/passwd",
		"",
		"not-an-ip",
		"10.0.0.1 accept\n ip daddr 0.0.0.0/0 accept",
	}
	for _, h := range invalid {
		if err := validateAllowlistHost(h); err == nil {
			t.Errorf("validateAllowlistHost(%q) should have returned error", h)
		}
	}
}

// ---------------------------------------------------------------------------
// Ingress "none" vs "restricted" produce different rules
// ---------------------------------------------------------------------------

func TestGenerateNftables_IngressNoneVsRestricted(t *testing.T) {
	noneRules := GenerateNftables(NetworkPolicy{
		TapDevice: "tap0",
		Egress:    "full",
		Ingress:   "none",
	})

	restrictedRules := GenerateNftables(NetworkPolicy{
		TapDevice: "tap0",
		Egress:    "full",
		Ingress:   "restricted",
	})

	// Both ingress modes should include a drop rule for the tap device.
	if !strings.Contains(noneRules, `oifname "tap0" drop`) {
		t.Error("ingress none: expected scoped drop rule for tap0")
	}
	if !strings.Contains(restrictedRules, `oifname "tap0" drop`) {
		t.Error("ingress restricted: expected scoped drop rule for tap0")
	}

	// Both should include established/related accept.
	if !strings.Contains(noneRules, "ct state established,related accept") {
		t.Error("ingress none: expected established/related accept rule")
	}
	if !strings.Contains(restrictedRules, "ct state established,related accept") {
		t.Error("ingress restricted: expected established/related accept rule")
	}

	// Verify that ingress "full" produces a different result from "none".
	fullRules := GenerateNftables(NetworkPolicy{
		TapDevice: "tap0",
		Egress:    "full",
		Ingress:   "full",
	})
	if fullRules == noneRules {
		t.Error("ingress 'full' and 'none' should produce different rules")
	}
	// Full ingress should contain an unconditional oifname accept.
	if !strings.Contains(fullRules, `oifname "tap0" accept`) {
		t.Error("ingress full: expected unconditional accept rule for tap0")
	}
}

// ---------------------------------------------------------------------------
// GenerateNftables: restricted egress without GatewayIP — unscoped DNS rules
// ---------------------------------------------------------------------------

func TestGenerateNftables_RestrictedNoGateway(t *testing.T) {
	rules := GenerateNftables(NetworkPolicy{
		TapDevice: "tap0",
		Egress:    "restricted",
		Allowlist: []string{"1.2.3.4"},
	})

	// Without GatewayIP, DNS rules should be unscoped (no ip daddr before port 53).
	lines := strings.Split(rules, "\n")
	foundDNS := false
	for _, line := range lines {
		if strings.Contains(line, "dport 53 accept") {
			foundDNS = true
			// Without gateway, the rule should NOT contain "daddr"
			// immediately before the port specification in DNS rules.
			if strings.Contains(line, "daddr") {
				t.Errorf("without GatewayIP, DNS rule should be unscoped, got: %s", line)
			}
		}
	}
	if !foundDNS {
		t.Error("restricted egress: expected DNS rules to be present")
	}
}

// ---------------------------------------------------------------------------
// nftAddrDirective: IPv4 vs IPv6 detection
// ---------------------------------------------------------------------------

func TestNftAddrDirective(t *testing.T) {
	tests := []struct {
		addr string
		want string
	}{
		{"10.0.0.1", "ip daddr"},
		{"192.168.1.0/24", "ip daddr"},
		{"::1", "ip6 daddr"},
		{"2001:db8::1", "ip6 daddr"},
		{"fe80::/10", "ip6 daddr"},
	}
	for _, tt := range tests {
		got := nftAddrDirective(tt.addr)
		if got != tt.want {
			t.Errorf("nftAddrDirective(%q) = %q, want %q", tt.addr, got, tt.want)
		}
	}
}
