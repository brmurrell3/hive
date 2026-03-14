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
// SEC-P3-003: IPv6 allowlist entries are filtered out (nftables is IPv4 only)
// ---------------------------------------------------------------------------

func TestGenerateNftables_IPv6AllowlistFiltered(t *testing.T) {
	rules, err := GenerateNftables(NetworkPolicy{
		TapDevice: "tap0",
		Egress:    "restricted",
		Allowlist: []string{"2001:db8::1", "fd00::/64"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// IPv6 entries should be filtered out by resolveAllowlistHosts.
	if strings.Contains(rules, "ip6 daddr") {
		t.Error("IPv6 allowlist: should NOT contain 'ip6 daddr' (IPv6 is filtered out)")
	}
	if strings.Contains(rules, "2001:db8::1") {
		t.Error("IPv6 allowlist: should NOT contain address 2001:db8::1 (IPv6 is filtered out)")
	}
	if strings.Contains(rules, "fd00::/64") {
		t.Error("IPv6 allowlist: should NOT contain CIDR fd00::/64 (IPv6 is filtered out)")
	}
}

// ---------------------------------------------------------------------------
// SEC-P3-003: Mixed IPv4+IPv6 allowlist — only IPv4 entries appear in rules
// ---------------------------------------------------------------------------

func TestGenerateNftables_MixedAllowlistOnlyIPv4(t *testing.T) {
	rules, err := GenerateNftables(NetworkPolicy{
		TapDevice: "tap0",
		Egress:    "restricted",
		Allowlist: []string{"10.0.0.1", "2001:db8::1", "192.168.1.0/24", "fd00::/64"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// IPv4 entries should be present.
	if !strings.Contains(rules, "10.0.0.1") {
		t.Error("mixed allowlist: expected IPv4 address 10.0.0.1 in rules")
	}
	if !strings.Contains(rules, "192.168.1.0/24") {
		t.Error("mixed allowlist: expected IPv4 CIDR 192.168.1.0/24 in rules")
	}
	// IPv6 entries should be filtered out.
	if strings.Contains(rules, "2001:db8::1") {
		t.Error("mixed allowlist: should NOT contain IPv6 address 2001:db8::1")
	}
	if strings.Contains(rules, "fd00::/64") {
		t.Error("mixed allowlist: should NOT contain IPv6 CIDR fd00::/64")
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

func TestResolveAllowlistHosts_IPv6Filtered(t *testing.T) {
	// SEC-P3-003: IPv6 addresses and CIDRs should be filtered out (not passed through).
	hosts := []string{"::1", "2001:db8::1", "fe80::/10", "fd00::/64"}
	resolved, err := resolveAllowlistHosts(hosts)
	if err != nil {
		t.Fatalf("resolveAllowlistHosts with IPv6 returned error: %v", err)
	}
	if len(resolved) != 0 {
		t.Errorf("expected all IPv6 entries to be filtered out, got %v", resolved)
	}
}

func TestResolveAllowlistHosts_MixedIPsAndCIDRs(t *testing.T) {
	// SEC-P3-003: Mixed list — IPv4 entries pass through, IPv6 entries are filtered.
	hosts := []string{"10.0.0.1", "192.168.0.0/16", "::1", "fd00::/64"}
	resolved, err := resolveAllowlistHosts(hosts)
	if err != nil {
		t.Fatalf("resolveAllowlistHosts with mixed list returned error: %v", err)
	}
	// Only the 2 IPv4 entries should remain.
	expectedResolved := []string{"10.0.0.1", "192.168.0.0/16"}
	if len(resolved) != len(expectedResolved) {
		t.Fatalf("expected %d resolved hosts (IPv4 only), got %d: %v", len(expectedResolved), len(resolved), resolved)
	}
	for i, h := range expectedResolved {
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
	// NET-H3: Empty device name fails tapDevicePattern validation and
	// returns a safe no-op command ("true" with nil args).
	cmd, args := CleanupNftables("")

	if cmd != "true" {
		t.Errorf("CleanupNftables command = %q, want %q for invalid input", cmd, "true")
	}
	if args != nil {
		t.Errorf("CleanupNftables args = %v, want nil for invalid input", args)
	}
}

func TestCleanupNftables_InvalidDeviceName(t *testing.T) {
	// NET-H3: Device names with shell/nftables injection characters should
	// return a safe no-op command.
	invalidNames := []string{
		"; drop table --",
		"tap0; rm -rf /",
		"tap device",
		"../../etc",
		"tap0\ndelete table inet foo",
		"a-very-long-device-name-exceeding-limit",
	}
	for _, name := range invalidNames {
		cmd, args := CleanupNftables(name)
		if cmd != "true" {
			t.Errorf("CleanupNftables(%q) command = %q, want %q", name, cmd, "true")
		}
		if args != nil {
			t.Errorf("CleanupNftables(%q) args = %v, want nil", name, args)
		}
	}
}

// ---------------------------------------------------------------------------
// TEST-COMBO: Ingress + Egress combination rules
// ---------------------------------------------------------------------------

func TestGenerateNftables_Combo_EgressNone_IngressNone(t *testing.T) {
	t.Parallel()

	rules, err := GenerateNftables(NetworkPolicy{
		TapDevice: "tap0",
		Egress:    "none",
		Ingress:   "none",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Egress chain: should have NATS accept, established/related, and drop.
	if !strings.Contains(rules, `iifname "tap0" tcp dport 4222 accept`) {
		t.Error("egressNone+ingressNone: egress chain missing NATS 4222 accept")
	}
	if !strings.Contains(rules, `iifname "tap0" ct state established,related accept`) {
		t.Error("egressNone+ingressNone: egress chain missing established/related accept")
	}
	if !strings.Contains(rules, `iifname "tap0" drop`) {
		t.Error("egressNone+ingressNone: egress chain missing drop rule")
	}

	// Egress chain must NOT have a blanket accept (that would be "full").
	lines := strings.Split(rules, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == `iifname "tap0" accept` {
			t.Error("egressNone+ingressNone: egress chain should NOT have blanket accept")
		}
	}

	// Ingress chain: "none" scopes established/related to NATS sport 4222 only.
	if !strings.Contains(rules, `oifname "tap0" tcp sport 4222 ct state established,related accept`) {
		t.Error("egressNone+ingressNone: ingress chain missing NATS-scoped established/related")
	}
	if !strings.Contains(rules, `oifname "tap0" drop`) {
		t.Error("egressNone+ingressNone: ingress chain missing drop rule")
	}

	// Ingress chain must NOT have a blanket accept.
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == `oifname "tap0" accept` {
			t.Error("egressNone+ingressNone: ingress chain should NOT have blanket accept")
		}
	}

	// Must have both chains.
	if !strings.Contains(rules, "chain egress {") {
		t.Error("egressNone+ingressNone: missing egress chain")
	}
	if !strings.Contains(rules, "chain ingress {") {
		t.Error("egressNone+ingressNone: missing ingress chain")
	}
}

func TestGenerateNftables_Combo_EgressRestricted_IngressRestricted(t *testing.T) {
	t.Parallel()

	rules, err := GenerateNftables(NetworkPolicy{
		TapDevice: "tap0",
		Egress:    "restricted",
		Ingress:   "restricted",
		Allowlist: []string{"10.0.0.1", "192.168.1.0/24"},
		GatewayIP: "10.0.0.254",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Egress chain: DNS scoped to gateway, NATS scoped to gateway, allowlist entries.
	if !strings.Contains(rules, "ip daddr 10.0.0.254 udp dport 53 accept") {
		t.Error("egressRestricted+ingressRestricted: missing DNS UDP rule scoped to gateway")
	}
	if !strings.Contains(rules, "ip daddr 10.0.0.254 tcp dport 53 accept") {
		t.Error("egressRestricted+ingressRestricted: missing DNS TCP rule scoped to gateway")
	}
	if !strings.Contains(rules, "ip daddr 10.0.0.254 tcp dport 4222 accept") {
		t.Error("egressRestricted+ingressRestricted: missing NATS rule scoped to gateway")
	}
	if !strings.Contains(rules, "ip daddr 10.0.0.1 accept") {
		t.Error("egressRestricted+ingressRestricted: missing allowlist entry 10.0.0.1")
	}
	if !strings.Contains(rules, "ip daddr 192.168.1.0/24 accept") {
		t.Error("egressRestricted+ingressRestricted: missing allowlist entry 192.168.1.0/24")
	}
	if !strings.Contains(rules, `iifname "tap0" ct state established,related accept`) {
		t.Error("egressRestricted+ingressRestricted: egress chain missing established/related")
	}
	if !strings.Contains(rules, `iifname "tap0" drop`) {
		t.Error("egressRestricted+ingressRestricted: egress chain missing drop")
	}

	// Ingress chain: "restricted" allows all established/related (not scoped to NATS).
	if !strings.Contains(rules, `oifname "tap0" ct state established,related accept`) {
		t.Error("egressRestricted+ingressRestricted: ingress chain missing established/related")
	}
	if strings.Contains(rules, `oifname "tap0" tcp sport 4222 ct state established,related`) {
		t.Error("egressRestricted+ingressRestricted: ingress should NOT scope established/related to sport 4222")
	}
	if !strings.Contains(rules, `oifname "tap0" drop`) {
		t.Error("egressRestricted+ingressRestricted: ingress chain missing drop")
	}
}

func TestGenerateNftables_Combo_EgressFull_IngressFull(t *testing.T) {
	t.Parallel()

	rules, err := GenerateNftables(NetworkPolicy{
		TapDevice: "tap0",
		Egress:    "full",
		Ingress:   "full",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Egress chain: blanket accept.
	if !strings.Contains(rules, `iifname "tap0" accept`) {
		t.Error("egressFull+ingressFull: egress chain missing blanket accept")
	}
	if !strings.Contains(rules, `iifname "tap0" drop`) {
		t.Error("egressFull+ingressFull: egress chain missing drop rule")
	}

	// Ingress chain: blanket accept.
	if !strings.Contains(rules, `oifname "tap0" accept`) {
		t.Error("egressFull+ingressFull: ingress chain missing blanket accept")
	}
	if !strings.Contains(rules, `oifname "tap0" drop`) {
		t.Error("egressFull+ingressFull: ingress chain missing drop rule")
	}

	// Should NOT have any NATS-specific or DNS-specific rules.
	if strings.Contains(rules, "dport 4222") {
		t.Error("egressFull+ingressFull: should NOT have explicit NATS port rules")
	}
	if strings.Contains(rules, "dport 53") {
		t.Error("egressFull+ingressFull: should NOT have explicit DNS port rules")
	}

	// Both chains should be present.
	if !strings.Contains(rules, "chain egress {") {
		t.Error("egressFull+ingressFull: missing egress chain")
	}
	if !strings.Contains(rules, "chain ingress {") {
		t.Error("egressFull+ingressFull: missing ingress chain")
	}
}

func TestGenerateNftables_Combo_EgressNone_IngressFull(t *testing.T) {
	t.Parallel()

	rules, err := GenerateNftables(NetworkPolicy{
		TapDevice: "tap0",
		Egress:    "none",
		Ingress:   "full",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Egress chain: locked down to NATS only.
	if !strings.Contains(rules, `iifname "tap0" tcp dport 4222 accept`) {
		t.Error("egressNone+ingressFull: egress chain missing NATS accept")
	}
	if !strings.Contains(rules, `iifname "tap0" ct state established,related accept`) {
		t.Error("egressNone+ingressFull: egress chain missing established/related")
	}
	if !strings.Contains(rules, `iifname "tap0" drop`) {
		t.Error("egressNone+ingressFull: egress chain missing drop")
	}

	// Ingress chain: fully open.
	if !strings.Contains(rules, `oifname "tap0" accept`) {
		t.Error("egressNone+ingressFull: ingress chain missing blanket accept")
	}
	if !strings.Contains(rules, `oifname "tap0" drop`) {
		t.Error("egressNone+ingressFull: ingress chain missing drop")
	}
}

func TestGenerateNftables_Combo_EgressFull_IngressNone(t *testing.T) {
	t.Parallel()

	rules, err := GenerateNftables(NetworkPolicy{
		TapDevice: "tap0",
		Egress:    "full",
		Ingress:   "none",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Egress chain: fully open.
	if !strings.Contains(rules, `iifname "tap0" accept`) {
		t.Error("egressFull+ingressNone: egress chain missing blanket accept")
	}
	if !strings.Contains(rules, `iifname "tap0" drop`) {
		t.Error("egressFull+ingressNone: egress chain missing drop")
	}

	// Ingress chain: only NATS-scoped established/related.
	if !strings.Contains(rules, `oifname "tap0" tcp sport 4222 ct state established,related accept`) {
		t.Error("egressFull+ingressNone: ingress chain missing NATS-scoped established/related")
	}
	if !strings.Contains(rules, `oifname "tap0" drop`) {
		t.Error("egressFull+ingressNone: ingress chain missing drop")
	}

	// Should NOT have blanket ingress accept.
	lines := strings.Split(rules, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == `oifname "tap0" accept` {
			t.Error("egressFull+ingressNone: ingress chain should NOT have blanket accept")
		}
	}
}

func TestGenerateNftables_Combo_EgressRestricted_IngressNone(t *testing.T) {
	t.Parallel()

	rules, err := GenerateNftables(NetworkPolicy{
		TapDevice: "tap0",
		Egress:    "restricted",
		Ingress:   "none",
		Allowlist: []string{"203.0.113.50"},
		GatewayIP: "10.0.0.1",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Egress chain: DNS and NATS scoped to gateway, allowlist entry, drop.
	if !strings.Contains(rules, "ip daddr 10.0.0.1 udp dport 53 accept") {
		t.Error("egressRestricted+ingressNone: missing scoped DNS UDP")
	}
	if !strings.Contains(rules, "ip daddr 10.0.0.1 tcp dport 4222 accept") {
		t.Error("egressRestricted+ingressNone: missing scoped NATS")
	}
	if !strings.Contains(rules, "ip daddr 203.0.113.50 accept") {
		t.Error("egressRestricted+ingressNone: missing allowlist entry")
	}
	if !strings.Contains(rules, `iifname "tap0" drop`) {
		t.Error("egressRestricted+ingressNone: egress chain missing drop")
	}

	// Ingress chain: NATS-scoped established/related only.
	if !strings.Contains(rules, `oifname "tap0" tcp sport 4222 ct state established,related accept`) {
		t.Error("egressRestricted+ingressNone: ingress chain missing NATS-scoped established/related")
	}
	if !strings.Contains(rules, `oifname "tap0" drop`) {
		t.Error("egressRestricted+ingressNone: ingress chain missing drop")
	}
}

func TestGenerateNftables_Combo_EgressNone_IngressRestricted(t *testing.T) {
	t.Parallel()

	rules, err := GenerateNftables(NetworkPolicy{
		TapDevice: "tap0",
		Egress:    "none",
		Ingress:   "restricted",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Egress chain: NATS only.
	if !strings.Contains(rules, `iifname "tap0" tcp dport 4222 accept`) {
		t.Error("egressNone+ingressRestricted: egress chain missing NATS accept")
	}
	if !strings.Contains(rules, `iifname "tap0" drop`) {
		t.Error("egressNone+ingressRestricted: egress chain missing drop")
	}

	// Ingress chain: all established/related (not scoped to NATS sport).
	if !strings.Contains(rules, `oifname "tap0" ct state established,related accept`) {
		t.Error("egressNone+ingressRestricted: ingress chain missing established/related")
	}
	if !strings.Contains(rules, `oifname "tap0" drop`) {
		t.Error("egressNone+ingressRestricted: ingress chain missing drop")
	}
}

func TestGenerateNftables_Combo_EgressFull_IngressRestricted(t *testing.T) {
	t.Parallel()

	rules, err := GenerateNftables(NetworkPolicy{
		TapDevice: "tap0",
		Egress:    "full",
		Ingress:   "restricted",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Egress chain: blanket accept.
	if !strings.Contains(rules, `iifname "tap0" accept`) {
		t.Error("egressFull+ingressRestricted: egress chain missing blanket accept")
	}
	if !strings.Contains(rules, `iifname "tap0" drop`) {
		t.Error("egressFull+ingressRestricted: egress chain missing drop")
	}

	// Ingress chain: general established/related (not NATS-scoped).
	if !strings.Contains(rules, `oifname "tap0" ct state established,related accept`) {
		t.Error("egressFull+ingressRestricted: ingress chain missing established/related")
	}
	if !strings.Contains(rules, `oifname "tap0" drop`) {
		t.Error("egressFull+ingressRestricted: ingress chain missing drop")
	}

	// Should NOT have blanket ingress accept (that would be "full").
	lines := strings.Split(rules, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == `oifname "tap0" accept` {
			t.Error("egressFull+ingressRestricted: ingress chain should NOT have blanket accept")
		}
	}
}

func TestGenerateNftables_Combo_EgressRestricted_IngressFull(t *testing.T) {
	t.Parallel()

	rules, err := GenerateNftables(NetworkPolicy{
		TapDevice: "tap0",
		Egress:    "restricted",
		Ingress:   "full",
		Allowlist: []string{"10.0.0.1"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Egress chain: DNS, NATS, allowlist, established/related, drop.
	if !strings.Contains(rules, "dport 53 accept") {
		t.Error("egressRestricted+ingressFull: egress chain missing DNS rules")
	}
	if !strings.Contains(rules, "dport 4222 accept") {
		t.Error("egressRestricted+ingressFull: egress chain missing NATS rule")
	}
	if !strings.Contains(rules, "ip daddr 10.0.0.1 accept") {
		t.Error("egressRestricted+ingressFull: egress chain missing allowlist entry")
	}
	if !strings.Contains(rules, `iifname "tap0" drop`) {
		t.Error("egressRestricted+ingressFull: egress chain missing drop")
	}

	// Ingress chain: blanket accept.
	if !strings.Contains(rules, `oifname "tap0" accept`) {
		t.Error("egressRestricted+ingressFull: ingress chain missing blanket accept")
	}
	if !strings.Contains(rules, `oifname "tap0" drop`) {
		t.Error("egressRestricted+ingressFull: ingress chain missing drop")
	}
}

func TestGenerateNftables_Combo_TableAndChainStructure(t *testing.T) {
	t.Parallel()

	// Verify the overall structure of generated rules for a mixed combo.
	rules, err := GenerateNftables(NetworkPolicy{
		TapDevice: "taptest01",
		Egress:    "restricted",
		Ingress:   "full",
		Allowlist: []string{"10.0.0.1"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Table name should be derived from the tap device.
	if !strings.Contains(rules, "table inet hive_taptest01 {") {
		t.Error("expected table name hive_taptest01")
	}

	// Both chains must exist with correct filter hook forward.
	egressIdx := strings.Index(rules, "chain egress {")
	ingressIdx := strings.Index(rules, "chain ingress {")
	if egressIdx < 0 {
		t.Fatal("missing egress chain")
	}
	if ingressIdx < 0 {
		t.Fatal("missing ingress chain")
	}
	// Egress chain should come before ingress chain.
	if egressIdx >= ingressIdx {
		t.Error("egress chain should appear before ingress chain")
	}

	// Both chains should have policy accept (not drop -- the scoped drop rules
	// are explicit, policy accept lets other interfaces pass through).
	if strings.Count(rules, "policy accept;") != 2 {
		t.Errorf("expected exactly 2 'policy accept;' directives, got %d", strings.Count(rules, "policy accept;"))
	}

	// The rules should end with a closing brace for the table.
	if !strings.HasSuffix(strings.TrimSpace(rules), "}") {
		t.Error("rules should end with closing brace")
	}
}
