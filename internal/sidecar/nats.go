package sidecar

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/brmurrell3/hive/internal/types"
	"github.com/nats-io/nats.go"
)

// connectNATS establishes a connection to the NATS server at the configured URL.
// It retries with exponential backoff to handle boot race conditions where
// the sidecar starts before hived's NATS server is ready.
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

	// Add authentication token if configured.
	if s.config.NATSToken != "" {
		opts = append(opts, nats.Token(s.config.NATSToken))
	}

	// Retry with exponential backoff for up to 60s.
	maxRetryDuration := 60 * time.Second
	backoff := 1 * time.Second
	maxBackoff := 30 * time.Second
	deadline := time.Now().Add(maxRetryDuration)

	var lastErr error
	for attempt := 1; ; attempt++ {
		nc, err := nats.Connect(s.config.NATSUrl, opts...)
		if err == nil {
			s.natsConn = nc
			s.logger.Info("connected to NATS", "url", s.config.NATSUrl, "attempt", attempt)
			return nil
		}
		lastErr = err

		if time.Now().After(deadline) {
			break
		}

		s.logger.Warn("NATS connection failed, retrying",
			"url", s.config.NATSUrl,
			"attempt", attempt,
			"backoff", backoff,
			"error", err,
		)

		time.Sleep(backoff)
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}

	return fmt.Errorf("connecting to NATS at %s after retries: %w", s.config.NATSUrl, lastErr)
}

// subscribeControl subscribes to the agent's control subject for receiving
// control plane commands (shutdown, restart, config updates, etc.).
func (s *Sidecar) subscribeControl() error {
	subject := fmt.Sprintf("hive.control.%s", s.agentID)

	var err error
	s.controlSub, err = s.natsConn.Subscribe(subject, func(msg *nats.Msg) {
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

	tier := s.config.Tier
	if tier == "" {
		tier = "vm"
	}

	payload := types.HealthPayload{
		Healthy:       healthy,
		UptimeSeconds: uptimeSeconds,
		Tier:          tier,
	}

	env := types.Envelope{
		ID:        types.NewUUID(),
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

// subscribeMemoryUpdates subscribes to hive.agent.{agentID}.memory for
// receiving MEMORY.md content pushed from hived when the file changes on the
// host. When a message arrives, the sidecar writes the content to
// {workspace}/MEMORY.md so the agent runtime can read it.
func (s *Sidecar) subscribeMemoryUpdates() error {
	subject := fmt.Sprintf("hive.agent.%s.memory", s.agentID)

	var err error
	s.memorySub, err = s.natsConn.Subscribe(subject, func(msg *nats.Msg) {
		s.logger.Debug("received memory update",
			"subject", msg.Subject,
			"size", len(msg.Data),
		)

		var env types.Envelope
		if err := json.Unmarshal(msg.Data, &env); err != nil {
			s.logger.Warn("failed to unmarshal memory update message",
				"error", err,
			)
			return
		}

		// The payload is the MEMORY.md content as a string.
		content, ok := env.Payload.(string)
		if !ok {
			s.logger.Warn("memory update payload is not a string",
				"payload_type", fmt.Sprintf("%T", env.Payload),
			)
			return
		}

		// Write the content to the workspace.
		workspace := s.config.WorkspacePath
		if workspace == "" {
			s.logger.Warn("no workspace path configured, cannot write MEMORY.md")
			return
		}

		memoryPath := workspace + "/MEMORY.md"
		if err := writeFileAtomic(memoryPath, []byte(content)); err != nil {
			s.logger.Error("failed to write MEMORY.md",
				"path", memoryPath,
				"error", err,
			)
			return
		}

		s.logger.Info("MEMORY.md updated from NATS push",
			"path", memoryPath,
			"size_bytes", len(content),
		)
	})
	if err != nil {
		return fmt.Errorf("subscribing to %s: %w", subject, err)
	}

	s.logger.Info("subscribed to memory updates", "subject", subject)
	return nil
}

// writeFileAtomic writes data to a file atomically by writing to a temporary
// file first and then renaming it. This prevents partial reads by the agent
// runtime if it reads the file while the sidecar is writing.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".memory-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("writing temp file: %w", err)
	}

	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("closing temp file: %w", err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("renaming temp file: %w", err)
	}

	return nil
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
