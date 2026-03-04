package types

import "time"

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
