// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build unit

package types

import (
	"encoding/json"
	"regexp"
	"testing"
)

var safeSubjectComponent = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func FuzzValidateSubjectComponent(f *testing.F) {
	// Valid components
	f.Add("agent-1")
	f.Add("my_agent")
	f.Add("AgentAlpha")
	f.Add("a")
	f.Add("abc123")
	f.Add("with-hyphen")
	f.Add("with_underscore")

	// NATS injection attempts from previous audit passes
	f.Add("agent.evil")              // dot = subject separator
	f.Add("agent>")                  // > = wildcard
	f.Add("agent*")                  // * = wildcard
	f.Add("agent evil")              // space
	f.Add("agent\tevil")             // tab
	f.Add("agent\nevil")             // newline
	f.Add("agent\x00evil")           // null byte
	f.Add("")                        // empty
	f.Add("../../../etc/passwd")     // path traversal
	f.Add(string(make([]byte, 256))) // over max length

	f.Fuzz(func(t *testing.T, value string) {
		err := ValidateSubjectComponent("test", value)
		if err != nil {
			return // rejection is fine
		}

		// Invariant: if validation passes, string must match safe regex
		if !safeSubjectComponent.MatchString(value) {
			t.Errorf("ValidateSubjectComponent passed %q but it doesn't match [a-zA-Z0-9_-]+", value)
		}

		// Invariant: must not exceed max length
		if len(value) > MaxSubjectComponentLength {
			t.Errorf("ValidateSubjectComponent passed %q (len=%d) but exceeds max %d", value, len(value), MaxSubjectComponentLength)
		}

		// Invariant: must not be empty
		if value == "" {
			t.Error("ValidateSubjectComponent passed empty string")
		}
	})
}

func FuzzEnvelopeValidate(f *testing.F) {
	// Valid envelope (the real-world input path is JSON deserialization)
	validEnv := `{"id":"test-id","from":"agent1","to":"team1.agent1","type":"task","timestamp":"2026-03-08T12:00:00Z","payload":"{}"}`
	f.Add([]byte(validEnv))

	// Minimal valid
	f.Add([]byte(`{"id":"x","from":"a","to":"b","type":"health","timestamp":"2026-03-08T00:00:00Z"}`))

	// Missing fields
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"id":"x"}`))
	f.Add([]byte(`{"id":"x","from":"a"}`))

	// Injection attempts
	f.Add([]byte(`{"id":"x","from":"a.b.c","to":"x","type":"task","timestamp":"2026-03-08T00:00:00Z"}`))
	f.Add([]byte(`{"id":"x","from":"a*","to":"x","type":"task","timestamp":"2026-03-08T00:00:00Z"}`))
	f.Add([]byte(`{"id":"x","from":"a>","to":"x","type":"task","timestamp":"2026-03-08T00:00:00Z"}`))

	// Oversized fields
	f.Add([]byte(`{"id":"` + string(make([]byte, 300)) + `","from":"a","to":"b","type":"task","timestamp":"2026-03-08T00:00:00Z"}`))

	// Invalid JSON
	f.Add([]byte(`not json`))
	f.Add([]byte(`{`))
	f.Add([]byte(``))

	// Control chars in correlation_id
	f.Add([]byte(`{"id":"x","from":"a","to":"b","type":"task","timestamp":"2026-03-08T00:00:00Z","correlation_id":"\u0001evil"}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var env Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			return // invalid JSON is fine
		}

		// Must not panic
		err := env.Validate()
		if err != nil {
			return // validation rejection is expected
		}

		// Invariants for valid envelopes:

		// From must be a safe subject component
		if !safeSubjectComponent.MatchString(env.From) {
			t.Errorf("Envelope.Validate() passed with unsafe From: %q", env.From)
		}

		// Type must be recognized
		if !ValidMessageTypes[env.Type] {
			t.Errorf("Envelope.Validate() passed with unknown Type: %q", env.Type)
		}

		// ID must not be empty
		if env.ID == "" {
			t.Error("Envelope.Validate() passed with empty ID")
		}

		// Payload must not exceed max
		if len(env.Payload) > MaxPayloadSize {
			t.Errorf("Envelope.Validate() passed with oversized payload: %d bytes", len(env.Payload))
		}
	})
}
