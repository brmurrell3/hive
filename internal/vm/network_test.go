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
	rules, err := GenerateNftables(NetworkPolicy{
		TapDevice: "tap0",
		Egress:    "none",
	})
	if err != nil {
		t.Fatal(err)
	}

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
	rules, err := GenerateNftables(NetworkPolicy{
		TapDevice: "tap0",
		Egress:    "restricted",
		Allowlist: []string{"10.0.0.1", "192.168.1.0/24"},
	})
	if err != nil {
		t.Fatal(err)
	}

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
	rules, err := GenerateNftables(NetworkPolicy{
		TapDevice: "tap0",
		Egress:    "full",
	})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(rules, `iifname "tap0" accept`) {
		t.Error("egress full: expected unconditional accept rule for tap0")
	}
}

// ---------------------------------------------------------------------------
// GenerateNftables: IPv6 allowlist entries use ip6 daddr
// ---------------------------------------------------------------------------

func TestGenerateNftables_IPv6Allowlist(t *testing.T) {
	rules, err := GenerateNftables(NetworkPolicy{
		TapDevice: "tap0",
		Egress:    "restricted",
		Allowlist: []string{"2001:db8::1", "fd00::/64"},
	})
	if err != nil {
		t.Fatal(err)
	}

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
	rules, err := GenerateNftables(NetworkPolicy{
		TapDevice: "tap0",
		Egress:    "restricted",
		GatewayIP: "10.0.0.1",
		Allowlist: []string{"1.2.3.4"},
	})
	if err != nil {
		t.Fatal(err)
	}

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
	rules, err := GenerateNftables(NetworkPolicy{
		TapDevice: "",
		Egress:    "none",
	})
	if err != nil {
		t.Fatal(err)
	}
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
	noneRules, err := GenerateNftables(NetworkPolicy{
		TapDevice: "tap0",
		Egress:    "full",
		Ingress:   "none",
	})
	if err != nil {
		t.Fatal(err)
	}

	restrictedRules, err := GenerateNftables(NetworkPolicy{
		TapDevice: "tap0",
		Egress:    "full",
		Ingress:   "restricted",
	})
	if err != nil {
		t.Fatal(err)
	}

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
	fullRules, err := GenerateNftables(NetworkPolicy{
		TapDevice: "tap0",
		Egress:    "full",
		Ingress:   "full",
	})
	if err != nil {
		t.Fatal(err)
	}
	if fullRules == noneRules {
		t.Error("ingress 'full' and 'none' should produce different rules")
	}
	// Full ingress should contain an unconditional oifname accept.
	if !strings.Contains(fullRules, `oifname "tap0" accept`) {
		t.Error("ingress full: expected unconditional accept rule for tap0")
	}

	// TEST-H2: Assert the semantic difference between "none" and "restricted".
	// Ingress "none" scopes established/related to NATS port 4222 only,
	// while "restricted" allows established/related for all traffic.
	if noneRules == restrictedRules {
		t.Error("ingress 'none' and 'restricted' should produce different rules")
	}

	// "none" must contain the NATS-sport-scoped established/related rule.
	if !strings.Contains(noneRules, `tcp sport 4222 ct state established,related accept`) {
		t.Error("ingress none: expected NATS-scoped (sport 4222) established/related rule")
	}

	// "restricted" must NOT contain the sport 4222 scope — it allows all established/related.
	if strings.Contains(restrictedRules, "sport 4222") {
		t.Error("ingress restricted: should NOT scope established/related to sport 4222")
	}

	// "restricted" must have a general established/related accept on oifname.
	if !strings.Contains(restrictedRules, `oifname "tap0" ct state established,related accept`) {
		t.Error("ingress restricted: expected general established/related accept on oifname tap0")
	}
}

// ---------------------------------------------------------------------------
// GenerateNftables: restricted egress without GatewayIP — unscoped DNS rules
// ---------------------------------------------------------------------------

func TestGenerateNftables_RestrictedNoGateway(t *testing.T) {
	rules, err := GenerateNftables(NetworkPolicy{
		TapDevice: "tap0",
		Egress:    "restricted",
		Allowlist: []string{"1.2.3.4"},
	})
	if err != nil {
		t.Fatal(err)
	}

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

// ---------------------------------------------------------------------------
// TEST-C1: resolveAllowlistHosts — IP/CIDR passthrough (no DNS lookup)
// ---------------------------------------------------------------------------

func TestResolveAllowlistHosts_IPPassthrough(t *testing.T) {
	// IPv4 addresses should pass through unchanged.
	hosts := []string{"10.0.0.1", "192.168.1.1", "255.255.255.255"}
	resolved, err := resolveAllowlistHosts(hosts)
	if err != nil {
		t.Fatalf("resolveAllowlistHosts with IPs returned error: %v", err)
	}
	if len(resolved) != len(hosts) {
		t.Fatalf("expected %d resolved hosts, got %d", len(hosts), len(resolved))
	}
	for i, h := range hosts {
		if resolved[i] != h {
			t.Errorf("resolved[%d] = %q, want %q", i, resolved[i], h)
		}
	}
}

func TestResolveAllowlistHosts_CIDRPassthrough(t *testing.T) {
	// CIDR ranges should pass through unchanged without DNS lookup.
	hosts := []string{"10.0.0.0/8", "192.168.1.0/24", "172.16.0.0/12"}
	resolved, err := resolveAllowlistHosts(hosts)
	if err != nil {
		t.Fatalf("resolveAllowlistHosts with CIDRs returned error: %v", err)
	}
	if len(resolved) != len(hosts) {
		t.Fatalf("expected %d resolved hosts, got %d", len(hosts), len(resolved))
	}
	for i, h := range hosts {
		if resolved[i] != h {
			t.Errorf("resolved[%d] = %q, want %q", i, resolved[i], h)
		}
	}
}

func TestResolveAllowlistHosts_IPv6Passthrough(t *testing.T) {
	// IPv6 addresses and CIDRs should pass through unchanged.
	hosts := []string{"::1", "2001:db8::1", "fe80::/10", "fd00::/64"}
	resolved, err := resolveAllowlistHosts(hosts)
	if err != nil {
		t.Fatalf("resolveAllowlistHosts with IPv6 returned error: %v", err)
	}
	if len(resolved) != len(hosts) {
		t.Fatalf("expected %d resolved hosts, got %d", len(hosts), len(resolved))
	}
	for i, h := range hosts {
		if resolved[i] != h {
			t.Errorf("resolved[%d] = %q, want %q", i, resolved[i], h)
		}
	}
}

func TestResolveAllowlistHosts_MixedIPsAndCIDRs(t *testing.T) {
	// Mixed list of IPv4 IPs, IPv6 IPs, and CIDRs should all pass through.
	hosts := []string{"10.0.0.1", "192.168.0.0/16", "::1", "fd00::/64"}
	resolved, err := resolveAllowlistHosts(hosts)
	if err != nil {
		t.Fatalf("resolveAllowlistHosts with mixed list returned error: %v", err)
	}
	if len(resolved) != len(hosts) {
		t.Fatalf("expected %d resolved hosts, got %d", len(hosts), len(resolved))
	}
	for i, h := range hosts {
		if resolved[i] != h {
			t.Errorf("resolved[%d] = %q, want %q", i, resolved[i], h)
		}
	}
}

func TestResolveAllowlistHosts_EmptyList(t *testing.T) {
	// Empty input should return nil/empty without error.
	resolved, err := resolveAllowlistHosts([]string{})
	if err != nil {
		t.Fatalf("resolveAllowlistHosts with empty list returned error: %v", err)
	}
	if len(resolved) != 0 {
		t.Errorf("expected empty resolved list, got %v", resolved)
	}
}

func TestResolveAllowlistHosts_NilList(t *testing.T) {
	// Nil input should return nil/empty without error.
	resolved, err := resolveAllowlistHosts(nil)
	if err != nil {
		t.Fatalf("resolveAllowlistHosts with nil list returned error: %v", err)
	}
	if len(resolved) != 0 {
		t.Errorf("expected empty resolved list, got %v", resolved)
	}
}

// ---------------------------------------------------------------------------
// TEST-C2: CleanupNftables
// ---------------------------------------------------------------------------

func TestCleanupNftables_ValidTapDevice(t *testing.T) {
	cmd, args := CleanupNftables("tap0")

	// Command should be "nft".
	if cmd != "nft" {
		t.Errorf("CleanupNftables command = %q, want %q", cmd, "nft")
	}

	// Args should delete the inet table named hive_<tapDevice>.
	expectedArgs := []string{"delete", "table", "inet", "hive_tap0"}
	if len(args) != len(expectedArgs) {
		t.Fatalf("CleanupNftables args length = %d, want %d", len(args), len(expectedArgs))
	}
	for i, a := range expectedArgs {
		if args[i] != a {
			t.Errorf("CleanupNftables args[%d] = %q, want %q", i, args[i], a)
		}
	}
}

func TestCleanupNftables_HyphenInDeviceName(t *testing.T) {
	// Hyphens in tap device names should be replaced with underscores
	// in the table name (to match GenerateNftables table naming).
	cmd, args := CleanupNftables("tap-device-1")

	if cmd != "nft" {
		t.Errorf("CleanupNftables command = %q, want %q", cmd, "nft")
	}

	expectedTable := "hive_tap_device_1"
	if len(args) < 4 {
		t.Fatalf("CleanupNftables args too short: %v", args)
	}
	if args[3] != expectedTable {
		t.Errorf("CleanupNftables table name = %q, want %q", args[3], expectedTable)
	}
}

func TestCleanupNftables_EmptyDeviceName(t *testing.T) {
	// Empty device name should produce an empty table suffix but still
	// return the "nft" command structure (caller is responsible for
	// validation before calling CleanupNftables).
	cmd, args := CleanupNftables("")

	if cmd != "nft" {
		t.Errorf("CleanupNftables command = %q, want %q", cmd, "nft")
	}

	// Table name with empty device = "hive_"
	expectedTable := "hive_"
	if len(args) < 4 {
		t.Fatalf("CleanupNftables args too short: %v", args)
	}
	if args[3] != expectedTable {
		t.Errorf("CleanupNftables table name = %q, want %q", args[3], expectedTable)
	}

	// Full args should still be well-formed.
	expectedArgs := []string{"delete", "table", "inet", "hive_"}
	for i, a := range expectedArgs {
		if args[i] != a {
			t.Errorf("CleanupNftables args[%d] = %q, want %q", i, args[i], a)
		}
	}
}
