// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package events

import "time"

// Event type constants for well-known NATS subjects.
const (
	// Agent lifecycle events.
	AgentCreated = "hive.events.agent.created"
	AgentStarted = "hive.events.agent.started"
	AgentStopped = "hive.events.agent.stopped"
	AgentFailed  = "hive.events.agent.failed"

	// Capability events.
	CapabilityRegistered = "hive.events.capability.registered"
	CapabilityInvoked    = "hive.events.capability.invoked"

	// Node events.
	NodeJoined = "hive.events.node.joined"
	NodeLeft   = "hive.events.node.left"
)

// Event is the payload published on event subjects.
type Event struct {
	Type      string                 `json:"type"`
	Timestamp time.Time              `json:"timestamp"`
	Source    string                 `json:"source"`
	Subject   string                 `json:"subject"`
	Data      map[string]interface{} `json:"data,omitempty"`
}
