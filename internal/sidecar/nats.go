// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package sidecar

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/brmurrell3/hive/internal/protocol"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/nats-io/nats.go"
)

const (
	// natsReconnectWait is the delay between NATS reconnection attempts.
	natsReconnectWait = 2 * time.Second

	// natsMaxRetryDuration is the total time allowed for initial NATS connection retries.
	natsMaxRetryDuration = 60 * time.Second

	// natsInitialBackoff is the starting backoff interval for connection retries.
	natsInitialBackoff = 1 * time.Second

	// natsMaxBackoff is the maximum backoff interval between connection retries.
	natsMaxBackoff = 30 * time.Second

	// defaultHealthInterval is the heartbeat publishing interval when none is configured.
	defaultHealthInterval = 30 * time.Second

	// natsDrainTimeout is how long to wait for a NATS drain to complete before forcing close.
	natsDrainTimeout = 5 * time.Second
)

// connectNATS establishes a connection to the NATS server at the configured URL.
// It retries with exponential backoff to handle boot race conditions where
// the sidecar starts before hived's NATS server is ready. The context allows
// the caller to cancel the retry loop during shutdown.
func (s *Sidecar) connectNATS(ctx context.Context) error {
	opts := []nats.Option{
		nats.Name(fmt.Sprintf("sidecar-%s", s.agentID)),
		nats.ReconnectWait(natsReconnectWait),
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

	// Retry with exponential backoff for up to the max retry duration.
	maxRetryDuration := natsMaxRetryDuration
	backoff := natsInitialBackoff
	maxBackoff := natsMaxBackoff
	deadline := time.Now().Add(maxRetryDuration)

	var lastErr error
	for attempt := 1; ; attempt++ {
		nc, err := nats.Connect(s.config.NATSUrl, opts...)
		if err == nil {
			s.mu.Lock()
			s.natsConn = nc
			s.mu.Unlock()
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

		// Use a context-aware sleep so the retry loop can be cancelled
		// during shutdown instead of blocking for the full backoff duration.
		select {
		case <-ctx.Done():
			return fmt.Errorf("connecting to NATS at %s cancelled: %w", s.config.NATSUrl, ctx.Err())
		case <-time.After(backoff):
		}

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
	subject := fmt.Sprintf(protocol.FmtAgentControl, s.agentID)

	var err error
	const maxControlMessageSize = 1024 * 1024 // 1MB
	s.controlSub, err = s.natsConn.Subscribe(subject, func(msg *nats.Msg) {
		if len(msg.Data) > maxControlMessageSize {
			s.logger.Warn("control message exceeds size limit", "size", len(msg.Data))
			return
		}

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

		if err := env.Validate(); err != nil {
			s.logger.Warn("invalid control message envelope",
				"error", err,
			)
			return
		}

		s.handleControlMessage(env, msg)
	})
	if err != nil {
		return fmt.Errorf("subscribing to %s: %w", subject, err)
	}

	s.logger.Info("subscribed to control subject", "subject", subject)
	return nil
}

// controlCommand represents the parsed payload of a control message.
type controlCommand struct {
	Command string `json:"command"`
}

// handleControlMessage processes an incoming control message envelope.
// Supported commands: "shutdown", "restart", "status", "config_reload".
func (s *Sidecar) handleControlMessage(env types.Envelope, msg *nats.Msg) {
	// Reject messages during shutdown to prevent processing after Stop() begins.
	select {
	case <-s.stopCh:
		s.logger.Debug("rejecting control message during shutdown", "type", env.Type)
		return
	default:
	}

	s.logger.Info("processing control message",
		"type", env.Type,
		"from", env.From,
		"id", env.ID,
	)

	if env.Type != types.MessageTypeControl {
		s.logger.Warn("unexpected message type on control subject",
			"type", env.Type,
		)
		return
	}

	// Parse the command from the payload.
	var cmd controlCommand
	if err := json.Unmarshal(env.Payload, &cmd); err != nil {
		s.logger.Warn("failed to unmarshal control command payload",
			"error", err,
		)
		return
	}

	switch cmd.Command {
	case "shutdown":
		s.logger.Info("shutdown command received, stopping sidecar")
		go s.Stop() //nolint:errcheck // fire-and-forget shutdown on command

	case "restart":
		s.logger.Info("restart command received, restarting runtime")
		if s.runtime != nil {
			// Cancel the old monitorRuntime goroutine before stopping the
			// runtime so it does not race with the new monitor.
			s.mu.RLock()
			oldCancel := s.monitorCancel
			s.mu.RUnlock()
			if oldCancel != nil {
				oldCancel()
			}

			if err := s.runtime.Stop(); err != nil {
				s.logger.Error("failed to stop runtime for restart", "error", err)
				return
			}
			if err := s.runtime.Start(); err != nil {
				s.logger.Error("failed to start runtime after restart", "error", err)
				return
			}
			// Create a new context for the new monitor goroutine.
			monitorCtx, monitorCancel := context.WithCancel(context.Background())
			s.mu.Lock()
			s.healthy = true
			s.monitorCancel = monitorCancel
			s.mu.Unlock()
			// Re-launch the runtime monitor so that if the restarted process
			// exits unexpectedly, the sidecar is marked unhealthy again.
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.monitorRuntime(monitorCtx)
			}()
			s.logger.Info("runtime restarted successfully")
		}

	case "status":
		s.logger.Info("status command received, reporting status")
		status := map[string]interface{}{
			"agent_id":       s.agentID,
			"team_id":        s.teamID,
			"healthy":        s.IsHealthy(),
			"uptime_seconds": int(time.Since(s.startTime).Seconds()),
		}
		if s.runtime != nil {
			status["runtime_running"] = s.runtime.IsRunning()
		}
		s.mu.RLock()
		nc := s.natsConn
		s.mu.RUnlock()
		if nc != nil {
			status["nats_connected"] = nc.IsConnected()
		}
		// Reply to the requesting message if it has a reply subject.
		if msg != nil && msg.Reply != "" {
			respBytes, err := json.Marshal(status)
			if err != nil {
				s.logger.Error("failed to marshal status response", "error", err)
				return
			}
			if err := msg.Respond(respBytes); err != nil {
				s.logger.Error("failed to send status response", "error", err)
			}
		}

	case "config_reload":
		s.logger.Info("config_reload command received")
		s.handleConfigReload(msg)

	default:
		s.logger.Warn("unknown control command", "command", cmd.Command)
	}
}

// handleConfigReload re-reads workspace configuration and MEMORY.md,
// publishes the updated content to NATS so hived is aware of local changes,
// and replies with the reload status if the message has a reply subject.
func (s *Sidecar) handleConfigReload(msg *nats.Msg) {
	workspace := s.config.WorkspacePath
	result := map[string]interface{}{
		"agent_id": s.agentID,
		"status":   "reloaded",
	}

	// Re-read MEMORY.md from the workspace and publish the updated content
	// to the agent's memory subject so hived and any other subscribers see it.
	if workspace != "" {
		const maxMemoryFileSize = 1 << 20 // 1MB
		memoryPath := filepath.Join(workspace, "MEMORY.md")

		// Check file size before reading to avoid loading oversized files
		// into memory.
		fi, statErr := os.Stat(memoryPath)
		//nolint:gocritic // ifElseChain — conditions are on different variables (statErr vs fi.Size)
		if statErr != nil {
			if !os.IsNotExist(statErr) {
				s.logger.Error("failed to stat MEMORY.md during config reload",
					"path", memoryPath,
					"error", statErr,
				)
				result["memory_status"] = "read_error"
			} else {
				s.logger.Debug("no MEMORY.md found during config reload", "path", memoryPath)
				result["memory_status"] = "not_found"
			}
		} else if fi.Size() > maxMemoryFileSize {
			s.logger.Warn("MEMORY.md too large during config reload, skipping publish",
				"size", fi.Size(),
				"max", maxMemoryFileSize,
			)
			result["memory_status"] = "too_large"
		} else {
			content, err := os.ReadFile(memoryPath)
			//nolint:gocritic // ifElseChain — conditions are on different variables (err vs content size)
			if err != nil {
				s.logger.Error("failed to read MEMORY.md during config reload",
					"path", memoryPath,
					"error", err,
				)
				result["memory_status"] = "read_error"
			} else if int64(len(content)) > maxMemoryFileSize {
				s.logger.Warn("MEMORY.md too large during config reload, skipping publish",
					"size", len(content),
					"max", maxMemoryFileSize,
				)
				result["memory_status"] = "too_large"
			} else {
				// Publish the content as a JSON-encoded string inside an Envelope
				// on the agent's memory subject so hived can pick it up.
				contentPayload, err := json.Marshal(string(content))
				if err != nil {
					s.logger.Error("failed to marshal MEMORY.md content", "error", err)
					result["memory_status"] = "marshal_error"
				} else {
					env := types.Envelope{
						ID:        types.NewUUID(),
						From:      s.agentID,
						To:        "hived",
						Type:      types.MessageTypeMemoryUpdate,
						Timestamp: time.Now().UTC(),
						Payload:   contentPayload,
					}
					data, err := json.Marshal(env)
					if err != nil {
						s.logger.Error("failed to marshal memory update envelope", "error", err)
						result["memory_status"] = "marshal_error"
					} else {
						subject := fmt.Sprintf(protocol.FmtAgentMemory, s.agentID)
						s.mu.RLock()
						nc := s.natsConn
						s.mu.RUnlock()
						if nc == nil {
							s.logger.Warn("cannot publish memory update, NATS connection closed")
							result["memory_status"] = "no_connection"
						} else if err := nc.Publish(subject, data); err != nil {
							s.logger.Error("failed to publish memory update", "error", err)
							result["memory_status"] = "publish_error"
						} else {
							s.logger.Info("MEMORY.md re-read and published on config reload",
								"path", memoryPath,
								"size_bytes", len(content),
							)
							result["memory_status"] = "published"
							result["memory_size_bytes"] = len(content)
						}
					}
				}
			}
		}
		result["workspace"] = workspace
	} else {
		result["memory_status"] = "no_workspace"
	}

	// Report capability count for visibility.
	s.mu.RLock()
	router := s.capRouter
	s.mu.RUnlock()
	if router != nil {
		result["capabilities_active"] = true
	}

	s.logger.Info("config reload completed", "agent_id", s.agentID)

	// Reply to the requesting message if it has a reply subject,
	// following the same pattern as the "status" handler.
	if msg != nil && msg.Reply != "" {
		respBytes, err := json.Marshal(result)
		if err != nil {
			s.logger.Error("failed to marshal config_reload response", "error", err)
			return
		}
		if err := msg.Respond(respBytes); err != nil {
			s.logger.Error("failed to send config_reload response", "error", err)
		}
	}
}

// startHeartbeat launches a goroutine that publishes periodic health heartbeats
// to the agent's health NATS subject. It runs until the sidecar's stop channel
// is closed.
func (s *Sidecar) startHeartbeat() {
	subject := fmt.Sprintf(protocol.FmtAgentHealth, s.agentID)
	interval := s.config.HealthInterval
	if interval == 0 {
		interval = defaultHealthInterval
	}

	s.logger.Info("starting heartbeat publisher",
		"subject", subject,
		"interval", interval,
	)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
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

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		s.logger.Error("failed to marshal heartbeat payload", "error", err)
		return
	}

	env := types.Envelope{
		ID:        types.NewUUID(),
		From:      s.agentID,
		To:        "hived",
		Type:      types.MessageTypeHealth,
		Timestamp: time.Now().UTC(),
		Payload:   payloadBytes,
	}

	data, err := json.Marshal(env)
	if err != nil {
		s.logger.Error("failed to marshal heartbeat", "error", err)
		return
	}

	s.mu.RLock()
	nc := s.natsConn
	s.mu.RUnlock()
	if nc == nil {
		s.logger.Warn("skipping heartbeat publish, NATS connection closed")
		return
	}

	if err := nc.Publish(subject, data); err != nil {
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
	subject := fmt.Sprintf(protocol.FmtAgentMemory, s.agentID)

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

		// The payload is the MEMORY.md content as a JSON string.
		// Since Payload is json.RawMessage, unmarshal it to get the string.
		var content string
		if err := json.Unmarshal(env.Payload, &content); err != nil {
			s.logger.Warn("memory update payload is not a string",
				"error", err,
			)
			return
		}

		// Reject oversized content before writing to disk.
		const maxMemoryFileSize = 1 << 20 // 1MB
		if len(content) > maxMemoryFileSize {
			s.logger.Warn("memory update too large, ignoring",
				"size", len(content),
				"max", maxMemoryFileSize,
			)
			return
		}

		// Write the content to the workspace.
		workspace := s.config.WorkspacePath
		if workspace == "" {
			s.logger.Warn("no workspace path configured, cannot write MEMORY.md")
			return
		}

		memoryPath := filepath.Join(workspace, "MEMORY.md")
		if err := writeFileAtomic(memoryPath, []byte(content), 0644); err != nil {
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
// file first, setting permissions, and then renaming it. This prevents partial
// reads by the agent runtime if it reads the file while the sidecar is writing.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
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

	// Sync to ensure data is flushed to disk before the rename. Without this,
	// a crash between Close and Rename could leave a zero-length or partial file.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("syncing temp file: %w", err)
	}

	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("closing temp file: %w", err)
	}

	// Set permissions before rename so the file is never visible with wrong mode.
	if err := os.Chmod(tmpName, perm); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("setting temp file permissions: %w", err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("renaming temp file: %w", err)
	}

	return nil
}

// closeNATS drains and closes the NATS connection gracefully with a timeout.
// The connection is nil-ed out after closing to prevent use-after-close.
// Uses a single write lock to atomically check and clear natsConn, eliminating
// the TOCTOU race between RLock check and Lock clear.
func (s *Sidecar) closeNATS() {
	s.mu.Lock()
	nc := s.natsConn
	s.natsConn = nil
	s.mu.Unlock()

	if nc == nil {
		return
	}

	done := make(chan struct{})
	go func() {
		if err := nc.Drain(); err != nil {
			s.logger.Warn("error draining NATS connection", "error", err)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(natsDrainTimeout):
		s.logger.Warn("NATS drain timed out, forcing close")
		nc.Close()
	}

	s.logger.Info("NATS connection closed")
}
