// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package types

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// MaxPayloadSize is the maximum allowed envelope payload size (2 MB).
const MaxPayloadSize = 2 << 20

// MaxCorrelationIDLength is the maximum allowed length for CorrelationID.
const MaxCorrelationIDLength = 256

// NewUUID generates a UUID v4 string using crypto/rand.
func NewUUID() string {
	var uuid [16]byte
	if _, err := rand.Read(uuid[:]); err != nil {
		// crypto/rand should never fail on supported platforms.
		// Panicking is preferable to returning a zero UUID, which would
		// cause all envelopes to share the same ID and break dedup.
		panic(fmt.Sprintf("crypto/rand failed in NewUUID: %v", err))
	}

	// Set version 4 (bits 12-15 of time_hi_and_version).
	uuid[6] = (uuid[6] & 0x0f) | 0x40
	// Set variant to RFC 4122 (bits 6-7 of clock_seq_hi_and_reserved).
	uuid[8] = (uuid[8] & 0x3f) | 0x80

	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		uuid[0:4],
		uuid[4:6],
		uuid[6:8],
		uuid[8:10],
		uuid[10:16],
	)
}

// Envelope is the standard NATS message envelope used for all Hive messages.
type Envelope struct {
	ID            string          `json:"id"`
	From          string          `json:"from"`
	To            string          `json:"to"`
	Type          MessageType     `json:"type"`
	Timestamp     time.Time       `json:"timestamp"`
	Payload       json.RawMessage `json:"payload"`
	ReplyTo       string          `json:"reply_to,omitempty"`
	CorrelationID string          `json:"correlation_id,omitempty"`
	UserToken     string          `json:"user_token,omitempty"`
}

// MessageType defines the type of a Hive message.
type MessageType string

const (
	MessageTypeTask               MessageType = "task"
	MessageTypeResult             MessageType = "result"
	MessageTypeBroadcast          MessageType = "broadcast"
	MessageTypeStatus             MessageType = "status"
	MessageTypeHealth             MessageType = "health"
	MessageTypeControl            MessageType = "control"
	MessageTypeCapabilityRequest  MessageType = "capability-request"
	MessageTypeCapabilityResponse MessageType = "capability-response"
	MessageTypeError              MessageType = "error"
	MessageTypeMemoryUpdate       MessageType = "memory-update"
	MessageTypeNodeHeartbeat      MessageType = "node-heartbeat"
)

// ValidMessageTypes is the set of valid message types.
var ValidMessageTypes = map[MessageType]bool{
	MessageTypeTask:               true,
	MessageTypeResult:             true,
	MessageTypeBroadcast:          true,
	MessageTypeStatus:             true,
	MessageTypeHealth:             true,
	MessageTypeControl:            true,
	MessageTypeCapabilityRequest:  true,
	MessageTypeCapabilityResponse: true,
	MessageTypeError:              true,
	MessageTypeMemoryUpdate:       true,
	MessageTypeNodeHeartbeat:      true,
}

// Validate checks that the envelope has the required fields populated:
// ID, From, To, Type must be non-empty, and Timestamp must be non-zero.
func (e *Envelope) Validate() error {
	if e.ID == "" {
		return fmt.Errorf("envelope validation: ID is required")
	}
	if len(e.ID) > 256 {
		return fmt.Errorf("envelope validation: ID exceeds maximum length of 256 characters")
	}
	if e.From == "" {
		return fmt.Errorf("envelope validation: From is required")
	}
	if err := ValidateSubjectComponent("From", e.From); err != nil {
		return fmt.Errorf("envelope validation: %w", err)
	}
	if e.To == "" {
		return fmt.Errorf("envelope validation: To is required")
	}
	if err := ValidateSubjectField("To", e.To); err != nil {
		return fmt.Errorf("envelope validation: %w", err)
	}
	if e.Type == "" {
		return fmt.Errorf("envelope validation: Type is required")
	}
	if !ValidMessageTypes[e.Type] {
		return fmt.Errorf("envelope validation: unknown message type %q", e.Type)
	}
	if e.Timestamp.IsZero() {
		return fmt.Errorf("envelope validation: Timestamp is required")
	}
	// Reject timestamps more than 5 minutes in the future to guard against clock skew attacks.
	if time.Until(e.Timestamp) > 5*time.Minute {
		return fmt.Errorf("envelope validation: Timestamp is too far in the future")
	}
	if time.Since(e.Timestamp) > 5*time.Minute {
		return fmt.Errorf("envelope validation: Timestamp is too far in the past")
	}
	if len(e.Payload) > MaxPayloadSize {
		return fmt.Errorf("envelope validation: Payload exceeds maximum size of %d bytes", MaxPayloadSize)
	}
	if len(e.CorrelationID) > MaxCorrelationIDLength {
		return fmt.Errorf("envelope validation: CorrelationID exceeds maximum length of %d characters", MaxCorrelationIDLength)
	}
	if e.CorrelationID != "" {
		for _, c := range e.CorrelationID {
			if c < 0x20 && c != '\t' {
				return fmt.Errorf("envelope validation: CorrelationID contains control characters")
			}
		}
	}
	if len(e.UserToken) > 4096 {
		return fmt.Errorf("envelope validation: UserToken exceeds maximum length of 4096 characters")
	}
	// Validate ReplyTo if present: must be a NATS inbox or a valid subject field.
	if e.ReplyTo != "" {
		if strings.HasPrefix(e.ReplyTo, "_INBOX.") {
			// Validate the suffix after _INBOX. contains only valid subject characters.
			suffix := e.ReplyTo[len("_INBOX."):]
			if err := ValidateSubjectField("ReplyTo (INBOX suffix)", suffix); err != nil {
				return fmt.Errorf("envelope validation: %w", err)
			}
		} else {
			if err := ValidateSubjectField("ReplyTo", e.ReplyTo); err != nil {
				return fmt.Errorf("envelope validation: %w", err)
			}
		}
	}
	return nil
}

// HealthPayload represents the payload of a health message.
type HealthPayload struct {
	Healthy        bool   `json:"healthy"`
	UptimeSeconds  int    `json:"uptime_seconds"`
	Tier           string `json:"tier"`
	CPUPercent     int    `json:"cpu_percent,omitempty"`
	MemoryUsed     int64  `json:"memory_used_bytes,omitempty"`
	BatteryPercent int    `json:"battery_percent,omitempty"`
	RSSI           int    `json:"rssi,omitempty"`
	FreeHeapBytes  int    `json:"free_heap_bytes,omitempty"`
}
