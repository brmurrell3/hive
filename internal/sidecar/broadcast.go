package sidecar

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/hivehq/hive/internal/types"
	"github.com/nats-io/nats.go"
)

// BroadcastHandler is called when a team broadcast message is received.
type BroadcastHandler func(env types.Envelope)

// SubscribeTeamBroadcast subscribes to team.{teamID}.broadcast for receiving
// broadcast messages from the lead agent.
func (s *Sidecar) SubscribeTeamBroadcast(handler BroadcastHandler) error {
	if s.teamID == "" {
		return fmt.Errorf("no team ID configured")
	}

	subject := fmt.Sprintf("team.%s.broadcast", s.teamID)
	sub, err := s.natsConn.Subscribe(subject, func(msg *nats.Msg) {
		var env types.Envelope
		if err := json.Unmarshal(msg.Data, &env); err != nil {
			s.logger.Warn("invalid broadcast message", "error", err)
			return
		}
		s.logger.Debug("broadcast received", "from", env.From, "team", s.teamID)
		handler(env)
	})
	if err != nil {
		return fmt.Errorf("subscribing to team broadcast: %w", err)
	}

	s.mu.Lock()
	s.broadcastSub = sub
	s.mu.Unlock()

	s.logger.Info("subscribed to team broadcast", "subject", subject)
	return nil
}

// PublishTeamBroadcast publishes a broadcast message to all team members.
// Only the lead agent should call this.
func (s *Sidecar) PublishTeamBroadcast(payload interface{}) error {
	if s.teamID == "" {
		return fmt.Errorf("no team ID configured")
	}

	subject := fmt.Sprintf("team.%s.broadcast", s.teamID)

	env := types.Envelope{
		ID:        newUUID(),
		From:      s.agentID,
		To:        subject,
		Type:      types.MessageTypeBroadcast,
		Timestamp: time.Now().UTC(),
		Payload:   payload,
	}

	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshaling broadcast: %w", err)
	}

	if err := s.natsConn.Publish(subject, data); err != nil {
		return fmt.Errorf("publishing broadcast: %w", err)
	}

	s.logger.Debug("broadcast published", "subject", subject)
	return nil
}
