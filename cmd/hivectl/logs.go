// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/brmurrell3/hive/internal/types"
	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
)

// logsCmd creates the top-level "logs" command that tails agent logs in
// real-time by subscribing to NATS log subjects (hive.logs.<agent-id>).
// Unlike "agents logs" which reads from local JSONL files, this command
// streams live log entries directly from the NATS bus.
func logsCmd() *cobra.Command {
	var (
		follow bool
		lines  int
		level  string
	)

	cmd := &cobra.Command{
		Use:   "logs AGENT_ID",
		Short: "Tail agent logs in real-time via NATS",
		Long: `Stream log entries for an agent by subscribing to the NATS log subject.

This command connects to the hived NATS server and subscribes to
hive.logs.<agent-id> to receive log entries as they are published.
By default it shows the last 50 entries and exits; use --follow for
continuous streaming.

Examples:
  # Show last 50 log entries for an agent
  hivectl logs my-agent

  # Follow logs continuously
  hivectl logs my-agent --follow

  # Show last 100 entries then follow
  hivectl logs my-agent --lines 100 --follow

  # Filter by log level
  hivectl logs my-agent --level error --follow`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]

			if err := types.ValidateSubjectComponent("agent_id", agentID); err != nil {
				return err
			}

			if lines < 0 {
				return fmt.Errorf("--lines must be non-negative (got %d)", lines)
			}

			return streamAgentLogs(cmd.Context(), agentID, follow, lines, level)
		},
	}

	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Follow log output continuously")
	cmd.Flags().IntVarP(&lines, "lines", "n", 50, "Number of recent log lines to show")
	cmd.Flags().StringVar(&level, "level", "", "Filter by log level (debug, info, warn, error)")

	return cmd
}

// natsLogEntry is a log entry received via NATS subscription.
type natsLogEntry struct {
	AgentID   string                 `json:"agent_id"`
	Timestamp time.Time              `json:"timestamp"`
	Level     string                 `json:"level"`
	Message   string                 `json:"message"`
	Fields    map[string]interface{} `json:"fields,omitempty"`
}

// streamAgentLogs connects to NATS and streams log entries for the given agent.
// It first requests recent log history (if available), then optionally follows
// new entries in real time.
func streamAgentLogs(parent context.Context, agentID string, follow bool, maxLines int, level string) error {
	nc, err := connectNATS(fmt.Sprintf("hivectl-logs-%s", agentID))
	if err != nil {
		return err
	}
	defer func() { _ = nc.Drain() }()

	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	// Handle signals for clean shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	// Request recent log entries from the log aggregator via the control plane.
	// The log query is sent as a control request to hived.
	recentEntries, err := fetchRecentLogs(nc, agentID, maxLines)
	if err != nil {
		// Non-fatal: we can still follow even if history fetch fails.
		fmt.Fprintf(os.Stderr, "Warning: could not fetch log history: %v\n", err)
	}

	// Filter and print recent entries.
	for _, entry := range recentEntries {
		if level != "" && !strings.EqualFold(entry.Level, level) {
			continue
		}
		printLogEntry(entry)
	}

	if !follow {
		if len(recentEntries) == 0 {
			fmt.Fprintf(os.Stderr, "No log entries found for agent %q\n", agentID)
		}
		return nil
	}

	// Subscribe to live log entries.
	subject := fmt.Sprintf("hive.logs.%s", agentID)
	msgCh := make(chan *nats.Msg, 256)

	sub, err := nc.ChanSubscribe(subject, msgCh)
	if err != nil {
		return fmt.Errorf("subscribing to %s: %w", subject, err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	fmt.Fprintf(os.Stderr, "Following logs for %s (press Ctrl+C to stop)...\n", agentID)

	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-msgCh:
			if !ok {
				return nil
			}
			entry, err := parseLogMessage(msg.Data)
			if err != nil {
				continue
			}
			if level != "" && !strings.EqualFold(entry.Level, level) {
				continue
			}
			printLogEntry(entry)
		}
	}
}

// fetchRecentLogs requests recent log entries from hived via the logs query
// control subject. If the aggregator is not available, it returns nil.
func fetchRecentLogs(nc *nats.Conn, agentID string, maxLines int) ([]natsLogEntry, error) {
	// Build a query request via the daemon control protocol.
	queryReq := map[string]interface{}{
		"agent_id": agentID,
		"limit":    maxLines,
	}

	payloadBytes, err := json.Marshal(queryReq)
	if err != nil {
		return nil, fmt.Errorf("marshaling log query: %w", err)
	}

	env := types.Envelope{
		ID:        types.NewUUID(),
		From:      "hivectl",
		To:        "hived",
		Type:      types.MessageTypeControl,
		Timestamp: time.Now().UTC(),
		Payload:   payloadBytes,
		UserToken: authToken,
	}

	data, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("marshaling envelope: %w", err)
	}

	msg, err := nc.Request("hive.ctl.logs.query", data, defaultNATSTimeout)
	if err != nil {
		// If the control endpoint is not available, return empty results.
		return nil, nil //nolint:nilerr // expected when log query endpoint is not available
	}

	var resp struct {
		Status  string         `json:"status"`
		Error   string         `json:"error,omitempty"`
		Entries []natsLogEntry `json:"entries,omitempty"`
	}
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		return nil, fmt.Errorf("parsing log query response: %w", err)
	}

	if resp.Error != "" {
		return nil, fmt.Errorf("log query error: %s", resp.Error)
	}

	return resp.Entries, nil
}

// parseLogMessage parses a raw NATS message into a log entry.
// It handles both direct LogEntry payloads and Envelope-wrapped payloads.
func parseLogMessage(data []byte) (natsLogEntry, error) {
	// Try parsing as an Envelope first (standard Hive message format).
	var env types.Envelope
	if err := json.Unmarshal(data, &env); err == nil && len(env.Payload) > 0 {
		var entry natsLogEntry
		if err := json.Unmarshal(env.Payload, &entry); err == nil {
			if entry.Timestamp.IsZero() {
				entry.Timestamp = env.Timestamp
			}
			if entry.AgentID == "" {
				entry.AgentID = env.From
			}
			return entry, nil
		}
	}

	// Fall back to parsing as a direct log entry.
	var entry natsLogEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return natsLogEntry{}, fmt.Errorf("parsing log entry: %w", err)
	}
	return entry, nil
}

// printLogEntry formats and prints a single log entry with colorized output.
func printLogEntry(entry natsLogEntry) {
	ts := entry.Timestamp.Format("2006-01-02 15:04:05")
	level := strings.ToUpper(entry.Level)
	if level == "" {
		level = "INFO"
	}

	// Pad level to fixed width for alignment.
	for len(level) < 5 {
		level += " "
	}

	coloredTS := styleTimestamp.Render(ts)
	coloredLevel := colorizeLogLevel(level)
	coloredAgent := styleAgentID.Render(entry.AgentID)

	fmt.Printf("%s %s [%s] %s", coloredTS, coloredLevel, coloredAgent, entry.Message)

	// Print any additional fields inline.
	if len(entry.Fields) > 0 {
		fieldParts := make([]string, 0, len(entry.Fields))
		for k, v := range entry.Fields {
			fieldParts = append(fieldParts, fmt.Sprintf("%s=%v", k, v))
		}
		fmt.Printf(" %s", styleTimestamp.Render(strings.Join(fieldParts, " ")))
	}

	fmt.Println()
}
