package types

import (
	"crypto/rand"
	"fmt"
	"time"
)

// NewUUID generates a UUID v4 string using crypto/rand.
func NewUUID() string {
	var uuid [16]byte
	if _, err := rand.Read(uuid[:]); err != nil {
		// crypto/rand should never fail on supported platforms.
		// Fall back to a zero UUID rather than panicking.
		return "00000000-0000-0000-0000-000000000000"
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
	ID            string      `json:"id"`
	From          string      `json:"from"`
	To            string      `json:"to"`
	Type          MessageType `json:"type"`
	Timestamp     time.Time   `json:"timestamp"`
	Payload       interface{} `json:"payload"`
	ReplyTo       string      `json:"reply_to,omitempty"`
	CorrelationID string      `json:"correlation_id,omitempty"`
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
