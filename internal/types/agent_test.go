// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build unit

package types

import "testing"

func TestAgentSpec_ResolvedMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		spec AgentSpec
		want string
	}{
		{
			name: "explicit managed",
			spec: AgentSpec{Mode: AgentModeManaged},
			want: AgentModeManaged,
		},
		{
			name: "explicit external",
			spec: AgentSpec{Mode: AgentModeExternal},
			want: AgentModeExternal,
		},
		{
			name: "infer managed from vm tier",
			spec: AgentSpec{Tier: "vm"},
			want: AgentModeManaged,
		},
		{
			name: "infer managed from firecracker backend",
			spec: AgentSpec{Runtime: AgentRuntime{Backend: "firecracker"}},
			want: AgentModeManaged,
		},
		{
			name: "infer managed from runtime command",
			spec: AgentSpec{Tier: "native", Runtime: AgentRuntime{Command: "/usr/bin/agent"}},
			want: AgentModeManaged,
		},
		{
			name: "infer external native no command",
			spec: AgentSpec{Tier: "native", Runtime: AgentRuntime{Type: "openclaw"}},
			want: AgentModeExternal,
		},
		{
			name: "infer managed empty spec (backward compat)",
			spec: AgentSpec{},
			want: AgentModeManaged,
		},
		{
			name: "explicit overrides inference",
			spec: AgentSpec{Mode: AgentModeExternal, Runtime: AgentRuntime{Command: "/usr/bin/agent"}},
			want: AgentModeExternal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.spec.ResolvedMode()
			if got != tt.want {
				t.Errorf("ResolvedMode() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAgentSpec_IsManaged(t *testing.T) {
	t.Parallel()

	managed := AgentSpec{Tier: "vm"}
	if !managed.IsManaged() {
		t.Error("expected vm tier to be managed")
	}

	external := AgentSpec{Tier: "native", Runtime: AgentRuntime{Type: "openclaw"}}
	if external.IsManaged() {
		t.Error("expected native tier with no command to NOT be managed")
	}
}
