package sidecar

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hivehq/hive/internal/types"
	"github.com/nats-io/nats.go"
)

// connectNATS establishes a connection to the NATS server at the configured URL.
// It sets up reconnection handlers for resilient communication.
func (s *Sidecar) connectNATS() error {
	opts := []nats.Option{
		nats.Name(fmt.Sprintf("sidecar-%s", s.agentID)),
		nats.ReconnectWait(2 * time.Second),
		nats.MaxReconnects(-1), // Reconnect indefinitely.
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			s.logger.Warn("NATS disconnected", "error", err)
		}),
		nats.ReconnectHandler(func(_ *nats.Conn) {
			s.logger.Info("NATS reconnected")
		}),
		nats.ClosedHandler(func(_ *nats.Conn) {
			s.logger.Info("NATS connection closed")
		}),
	}

	nc, err := nats.Connect(s.config.NATSUrl, opts...)
	if err != nil {
		return fmt.Errorf("connecting to NATS at %s: %w", s.config.NATSUrl, err)
	}

	s.natsConn = nc
	s.logger.Info("connected to NATS", "url", s.config.NATSUrl)
	return nil
}

// subscribeControl subscribes to the agent's control subject for receiving
// control plane commands (shutdown, restart, config updates, etc.).
func (s *Sidecar) subscribeControl() error {
	subject := fmt.Sprintf("hive.control.%s", s.agentID)

	_, err := s.natsConn.Subscribe(subject, func(msg *nats.Msg) {
		s.logger.Info("received control message",
			"subject", msg.Subject,
			"size", len(msg.Data),
		)

		var env types.Envelope
		if err := json.Unmarshal(msg.Data, &env); err != nil {
			s.logger.Warn("failed to unmarshal control message",
				"error", err,
			)
			return
		}

		s.handleControlMessage(env)
	})
	if err != nil {
		return fmt.Errorf("subscribing to %s: %w", subject, err)
	}

	s.logger.Info("subscribed to control subject", "subject", subject)
	return nil
}

// handleControlMessage processes an incoming control message envelope.
func (s *Sidecar) handleControlMessage(env types.Envelope) {
	s.logger.Info("processing control message",
		"type", env.Type,
		"from", env.From,
		"id", env.ID,
	)

	// Control message handling will be expanded in M5.
	// For now, log the message type.
	switch env.Type {
	case types.MessageTypeControl:
		s.logger.Info("control command received", "id", env.ID)
	default:
		s.logger.Warn("unexpected message type on control subject",
			"type", env.Type,
		)
	}
}

// startHeartbeat launches a goroutine that publishes periodic health heartbeats
// to the agent's health NATS subject. It runs until the sidecar's stop channel
// is closed.
func (s *Sidecar) startHeartbeat() {
	subject := fmt.Sprintf("hive.health.%s", s.agentID)
	interval := s.config.HealthInterval
	if interval == 0 {
		interval = 30 * time.Second
	}

	s.logger.Info("starting heartbeat publisher",
		"subject", subject,
		"interval", interval,
	)

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		// Publish an initial heartbeat immediately.
		s.publishHeartbeat(subject)

		for {
			select {
			case <-ticker.C:
				s.publishHeartbeat(subject)
			case <-s.stopCh:
				s.logger.Info("heartbeat publisher stopped")
				return
			}
		}
	}()
}

// publishHeartbeat constructs and publishes a single health heartbeat message
// on the given NATS subject.
func (s *Sidecar) publishHeartbeat(subject string) {
	healthy := s.IsHealthy()
	uptimeSeconds := int(time.Since(s.startTime).Seconds())

	payload := types.HealthPayload{
		Healthy:       healthy,
		UptimeSeconds: uptimeSeconds,
		Tier:          "vm", // Sidecar in standalone mode implies VM tier.
	}

	env := types.Envelope{
		ID:        newUUID(),
		From:      s.agentID,
		To:        "hived",
		Type:      types.MessageTypeHealth,
		Timestamp: time.Now().UTC(),
		Payload:   payload,
	}

	data, err := json.Marshal(env)
	if err != nil {
		s.logger.Error("failed to marshal heartbeat", "error", err)
		return
	}

	if err := s.natsConn.Publish(subject, data); err != nil {
		s.logger.Error("failed to publish heartbeat",
			"subject", subject,
			"error", err,
		)
		return
	}

	s.logger.Debug("heartbeat published",
		"subject", subject,
		"healthy", healthy,
		"uptime_seconds", uptimeSeconds,
	)
}

// closeNATS drains and closes the NATS connection gracefully.
func (s *Sidecar) closeNATS() {
	if s.natsConn == nil {
		return
	}

	// Drain ensures in-flight messages are processed before closing.
	if err := s.natsConn.Drain(); err != nil {
		s.logger.Warn("error draining NATS connection", "error", err)
	}

	s.logger.Info("NATS connection closed")
}

// newUUID generates a UUID v4 string using crypto/rand.
func newUUID() string {
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
