// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

// NOTE: Events are currently published using fire-and-forget NATS publish.
// During NATS reconnection, events may be silently dropped. For durable
// event delivery, consider migrating to JetStream publish with acknowledgment.

// Package events provides cluster lifecycle event publishing over NATS for node and agent state changes.
package events

import (
	"encoding/json"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
)

// Publisher wraps a NATS connection for publishing typed events.
type Publisher struct {
	nc     *nats.Conn
	source string
	logger *slog.Logger
}

// NewPublisher creates a new event publisher. The source identifies the
// component emitting events (e.g., "hived", "agent-manager").
func NewPublisher(nc *nats.Conn, source string, logger *slog.Logger) *Publisher {
	return &Publisher{
		nc:     nc,
		source: source,
		logger: logger,
	}
}

// Publish sends an event on the given subject.
func (p *Publisher) Publish(subject string, data map[string]interface{}) {
	evt := Event{
		Type:      subject,
		Timestamp: time.Now().UTC(),
		Source:    p.source,
		Subject:   subject,
		Data:      data,
	}

	payload, err := json.Marshal(evt)
	if err != nil {
		p.logger.Error("failed to marshal event", "subject", subject, "error", err)
		return
	}

	if err := p.nc.Publish(subject, payload); err != nil {
		p.logger.Error("failed to publish event (fire-and-forget, event may be lost)",
			"subject", subject,
			"error", err,
		)
		return
	}

	p.logger.Debug("event published", "subject", subject)
}

// AgentCreated publishes an agent created event.
func (p *Publisher) AgentCreated(agentID, team string) {
	p.Publish(AgentCreated, map[string]interface{}{
		"agent_id": agentID,
		"team":     team,
	})
}

// AgentStarted publishes an agent started event.
func (p *Publisher) AgentStarted(agentID string) {
	p.Publish(AgentStarted, map[string]interface{}{
		"agent_id": agentID,
	})
}

// AgentStopped publishes an agent stopped event.
func (p *Publisher) AgentStopped(agentID string) {
	p.Publish(AgentStopped, map[string]interface{}{
		"agent_id": agentID,
	})
}

// AgentFailed publishes an agent failed event.
func (p *Publisher) AgentFailed(agentID, reason string) {
	p.Publish(AgentFailed, map[string]interface{}{
		"agent_id": agentID,
		"reason":   reason,
	})
}

// CapabilityRegistered publishes a capability registration event.
func (p *Publisher) CapabilityRegistered(agentID, capability string) {
	p.Publish(CapabilityRegistered, map[string]interface{}{
		"agent_id":   agentID,
		"capability": capability,
	})
}

// CapabilityInvoked publishes a capability invocation event.
func (p *Publisher) CapabilityInvoked(from, to, capability string) {
	p.Publish(CapabilityInvoked, map[string]interface{}{
		"from":       from,
		"to":         to,
		"capability": capability,
	})
}

// NodeJoined publishes a node joined event.
func (p *Publisher) NodeJoined(nodeID string) {
	p.Publish(NodeJoined, map[string]interface{}{
		"node_id": nodeID,
	})
}

// NodeLeft publishes a node left/offline event.
func (p *Publisher) NodeLeft(nodeID string) {
	p.Publish(NodeLeft, map[string]interface{}{
		"node_id": nodeID,
	})
}
