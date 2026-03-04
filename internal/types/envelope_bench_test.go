// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build unit

package types

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// makeValidEnvelope returns a fully-populated Envelope that passes Validate().
func makeValidEnvelope(payloadSize int) *Envelope {
	payload := json.RawMessage(`"` + strings.Repeat("x", payloadSize) + `"`)
	return &Envelope{
		ID:            NewUUID(),
		From:          "agent-sender",
		To:            "hive.control",
		Type:          MessageTypeTask,
		Timestamp:     time.Now(),
		Payload:       payload,
		CorrelationID: "corr-123",
		ReplyTo:       "_INBOX.abcdef",
	}
}

// BenchmarkEnvelopeValidate benchmarks the Validate hot path with different
// payload sizes. Validate is called on every inbound message, so it must
// stay cheap even as payload sizes grow.
func BenchmarkEnvelopeValidate(b *testing.B) {
	sizes := []struct {
		name  string
		bytes int
	}{
		{"payload=small(64B)", 64},
		{"payload=medium(4KB)", 4 * 1024},
		{"payload=large(64KB)", 64 * 1024},
	}

	for _, sz := range sizes {
		b.Run(sz.name, func(b *testing.B) {
			env := makeValidEnvelope(sz.bytes)

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// Refresh Timestamp so the future/past checks always pass.
				env.Timestamp = time.Now()
				if err := env.Validate(); err != nil {
					b.Fatalf("Validate: %v", err)
				}
			}
		})
	}
}

// BenchmarkEnvelopeMarshal benchmarks JSON marshalling of an Envelope with
// different payload sizes. This is the encoding path used before publishing
// to NATS.
func BenchmarkEnvelopeMarshal(b *testing.B) {
	sizes := []struct {
		name  string
		bytes int
	}{
		{"payload=small(64B)", 64},
		{"payload=medium(4KB)", 4 * 1024},
		{"payload=large(64KB)", 64 * 1024},
		{"payload=max(512KB)", 512 * 1024},
	}

	for _, sz := range sizes {
		b.Run(sz.name, func(b *testing.B) {
			env := makeValidEnvelope(sz.bytes)

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				data, err := json.Marshal(env)
				if err != nil {
					b.Fatalf("Marshal: %v", err)
				}
				_ = data
			}
		})
	}
}

// BenchmarkEnvelopeUnmarshal benchmarks JSON unmarshalling of a pre-serialised
// Envelope. This is the decoding path exercised by every NATS subscriber.
func BenchmarkEnvelopeUnmarshal(b *testing.B) {
	sizes := []struct {
		name  string
		bytes int
	}{
		{"payload=small(64B)", 64},
		{"payload=medium(4KB)", 4 * 1024},
		{"payload=large(64KB)", 64 * 1024},
		{"payload=max(512KB)", 512 * 1024},
	}

	for _, sz := range sizes {
		b.Run(sz.name, func(b *testing.B) {
			env := makeValidEnvelope(sz.bytes)
			data, err := json.Marshal(env)
			if err != nil {
				b.Fatalf("setup Marshal: %v", err)
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var out Envelope
				if err := json.Unmarshal(data, &out); err != nil {
					b.Fatalf("Unmarshal: %v", err)
				}
				_ = out
			}
		})
	}
}

// BenchmarkEnvelopeMarshalUnmarshalRoundtrip benchmarks the full round-trip
// (Marshal + Unmarshal + Validate) as experienced by a relay or bridge that
// decodes, validates, and re-encodes messages.
func BenchmarkEnvelopeMarshalUnmarshalRoundtrip(b *testing.B) {
	sizes := []struct {
		name  string
		bytes int
	}{
		{"payload=small(64B)", 64},
		{"payload=medium(4KB)", 4 * 1024},
		{"payload=large(64KB)", 64 * 1024},
	}

	for _, sz := range sizes {
		b.Run(sz.name, func(b *testing.B) {
			env := makeValidEnvelope(sz.bytes)

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				env.Timestamp = time.Now()

				data, err := json.Marshal(env)
				if err != nil {
					b.Fatalf("Marshal: %v", err)
				}

				var out Envelope
				if err := json.Unmarshal(data, &out); err != nil {
					b.Fatalf("Unmarshal: %v", err)
				}

				if err := out.Validate(); err != nil {
					b.Fatalf("Validate: %v", err)
				}
			}
		})
	}
}

// BenchmarkNewUUID benchmarks UUID generation, which is called whenever a new
// Envelope is constructed.
func BenchmarkNewUUID(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := NewUUID()
		_ = id
	}
}
