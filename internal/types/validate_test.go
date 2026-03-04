// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build unit

package types

import (
	"strings"
	"testing"
)

func TestValidateSubjectComponent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		field   string
		value   string
		wantErr bool
	}{
		// Valid IDs
		{"simple alpha", "agent_id", "myagent", false},
		{"alphanumeric", "agent_id", "agent123", false},
		{"with hyphens", "agent_id", "my-agent", false},
		{"with underscores", "agent_id", "my_agent", false},
		{"mixed case", "agent_id", "MyAgent", false},
		{"all valid chars", "agent_id", "My-Agent_123", false},

		// Invalid IDs
		{"empty string", "agent_id", "", true},
		{"contains dot", "agent_id", "agent.name", true},
		{"contains wildcard star", "agent_id", "agent*", true},
		{"contains greater-than", "agent_id", "agent>", true},
		{"contains space", "agent_id", "agent name", true},
		{"contains slash", "agent_id", "agent/name", true},
		{"contains backslash", "agent_id", "agent\\name", true},
		{"contains newline", "agent_id", "agent\nname", true},
		{"contains tab", "agent_id", "agent\tname", true},
		{"path traversal dotdot", "agent_id", "../etc/passwd", true},
		{"contains colon", "agent_id", "agent:name", true},
		{"contains at sign", "agent_id", "agent@name", true},
		{"unicode characters", "agent_id", "agent\u00e9", true},
		{"max length", "agent_id", strings.Repeat("a", MaxSubjectComponentLength), false},
		{"over max length", "agent_id", strings.Repeat("a", MaxSubjectComponentLength+1), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateSubjectComponent(tt.field, tt.value)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateSubjectComponent(%q, %q) error = %v, wantErr %v", tt.field, tt.value, err, tt.wantErr)
			}
		})
	}
}

func TestValidatePathComponent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		field   string
		value   string
		wantErr bool
	}{
		// Valid path components
		{"simple alpha", "agent_id", "myagent", false},
		{"alphanumeric", "agent_id", "agent123", false},
		{"with hyphens", "agent_id", "my-agent", false},
		{"with underscores", "agent_id", "my_agent", false},

		// Invalid - same as subject component
		{"empty string", "agent_id", "", true},
		{"contains dot", "agent_id", "agent.name", true},
		{"contains space", "agent_id", "agent name", true},

		// Path traversal attacks
		{"dot-dot traversal", "agent_id", "..", true},
		{"path with slash", "agent_id", "agent/name", true},
		{"path with backslash", "agent_id", "agent\\name", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidatePathComponent(tt.field, tt.value)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidatePathComponent(%q, %q) error = %v, wantErr %v", tt.field, tt.value, err, tt.wantErr)
			}
		})
	}
}

func TestValidateSubjectComponent_ErrorMessages(t *testing.T) {
	t.Parallel()
	err := ValidateSubjectComponent("agent_id", "")
	if err == nil {
		t.Fatal("expected error for empty string")
	}
	if err.Error() != "agent_id must not be empty" {
		t.Errorf("unexpected error message: %v", err)
	}

	err = ValidateSubjectComponent("team_id", "bad.value")
	if err == nil {
		t.Fatal("expected error for dotted value")
	}
	expected := `team_id contains invalid characters (only alphanumeric, underscore, hyphen allowed): "bad.value"`
	if err.Error() != expected {
		t.Errorf("unexpected error message: got %q, want %q", err.Error(), expected)
	}
}

func TestValidatePathComponent_ErrorMessages(t *testing.T) {
	t.Parallel()
	err := ValidatePathComponent("agent_id", "")
	if err == nil {
		t.Fatal("expected error for empty string")
	}
	if err.Error() != "agent_id must not be empty" {
		t.Errorf("unexpected error message: %v", err)
	}
}
