// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package sidecar

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/brmurrell3/hive/internal/protocol"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/nats-io/nats.go"
)

// BroadcastHandler is called when a team broadcast message is received.
type BroadcastHandler func(env types.Envelope)

// SubscribeTeamBroadcast subscribes to team.{teamID}.broadcast for receiving
// broadcast messages from the lead agent.
func (s *Sidecar) SubscribeTeamBroadcast(handler BroadcastHandler) (*nats.Subscription, error) {
	if s.teamID == "" {
		return nil, fmt.Errorf("no team ID configured")
	}
	s.mu.RLock()
	nc := s.natsConn
	s.mu.RUnlock()
	if nc == nil {
		return nil, fmt.Errorf("NATS connection not established")
	}

	subject := fmt.Sprintf(protocol.FmtTeamBroadcast, s.teamID)
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		defer func() {
			if r := recover(); r != nil {
				s.logger.Error("panic in broadcast handler", "recover", r)
			}
		}()
		var env types.Envelope
		if err := json.Unmarshal(msg.Data, &env); err != nil {
			s.logger.Warn("invalid broadcast message", "error", err)
			return
		}
		s.logger.Debug("broadcast received", "from", env.From, "team", s.teamID)
		handler(env)
	})
	if err != nil {
		return nil, fmt.Errorf("subscribing to team broadcast: %w", err)
	}

	s.mu.Lock()
	s.broadcastSub = sub
	s.mu.Unlock()

	s.logger.Info("subscribed to team broadcast", "subject", subject)
	return sub, nil
}

// PublishTeamBroadcast publishes a broadcast message to all team members.
// Only the lead agent should call this.
func (s *Sidecar) PublishTeamBroadcast(payload interface{}) error {
	if s.teamID == "" {
		return fmt.Errorf("no team ID configured")
	}
	s.mu.RLock()
	nc := s.natsConn
	s.mu.RUnlock()
	if nc == nil {
		return fmt.Errorf("NATS connection not established")
	}

	subject := fmt.Sprintf(protocol.FmtTeamBroadcast, s.teamID)

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling broadcast payload: %w", err)
	}

	env := types.Envelope{
		ID:        types.NewUUID(),
		From:      s.agentID,
		To:        s.teamID,
		Type:      types.MessageTypeBroadcast,
		Timestamp: time.Now().UTC(),
		Payload:   payloadBytes,
	}

	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshaling broadcast: %w", err)
	}

	if err := nc.Publish(subject, data); err != nil {
		return fmt.Errorf("publishing broadcast: %w", err)
	}

	s.logger.Debug("broadcast published", "subject", subject)
	return nil
}
