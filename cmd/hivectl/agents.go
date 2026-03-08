// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/brmurrell3/hive/internal/config"
	"github.com/brmurrell3/hive/internal/protocol"
	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/templates"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
)

const (
	runtimeOpenclaw = "openclaw"
	runtimeCustom   = "custom"
	extJSONL        = ".jsonl"
)

func agentsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agents",
		Short: "Manage agents",
	}

	cmd.AddCommand(agentsListCmd())
	cmd.AddCommand(agentsStatusCmd())
	cmd.AddCommand(agentsStartCmd())
	cmd.AddCommand(agentsStopCmd())
	cmd.AddCommand(agentsDestroyCmd())
	cmd.AddCommand(agentsRestartCmd())
	cmd.AddCommand(agentsLogsCmd())
	cmd.AddCommand(agentsExecCmd())
	cmd.AddCommand(agentsCapabilitiesCmd())
	cmd.AddCommand(agentsAddCmd())

	return cmd
}

func agentsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all agents with their current state",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(func(client *DaemonClient) error {
				agents, err := client.AgentList()
				if err != nil {
					return fmt.Errorf("listing agents: %w", err)
				}

				w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
				fmt.Fprintln(w, "AGENT_ID\tTEAM\tSTATE\tUPTIME")

				for _, a := range agents {
					uptime := ""
					if a.Status == state.AgentStatusRunning && !a.StartedAt.IsZero() {
						uptime = formatDuration(time.Since(a.StartedAt))
					}
					team := a.Team
					if team == "" {
						team = "-"
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", a.ID, team, a.Status, uptime)
				}

				w.Flush()
				return nil
			})
		},
	}
}

func agentsStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status AGENT_ID",
		Short: "Show detailed status for an agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]

			if err := types.ValidateSubjectComponent("agent_id", agentID); err != nil {
				return err
			}

			return withClient(func(client *DaemonClient) error {
				a, err := client.AgentStatus(agentID)
				if err != nil {
					return fmt.Errorf("getting agent status: %w", err)
				}

				data, err := json.MarshalIndent(a, "", "  ")
				if err != nil {
					return fmt.Errorf("marshaling agent state: %w", err)
				}
				fmt.Println(string(data))
				return nil
			})
		},
	}
}

// simpleAgentCmd creates a cobra command that validates an agent ID,
// sends an agent control request, and prints a success message.
func simpleAgentCmd(use, short, long string, action func(c *DaemonClient, id string) error, pastTense string) *cobra.Command {
	return simpleResourceCmd(use+" AGENT_ID", short, long, "agent_id", "Agent", action, pastTense)
}

func agentsStartCmd() *cobra.Command {
	return simpleAgentCmd("start", "Start an agent",
		`Start a stopped or newly registered agent by sending a start request to hived.
The daemon launches the agent's VM or process and transitions it to the
running state; use "agents status" to confirm.

Examples:
  hivectl agents start assistant
  # Output: Agent assistant started

  hivectl agents start analyst --cluster-root ./my-cluster
  # Output: Agent analyst started`,
		(*DaemonClient).StartAgent, "started")
}

func agentsStopCmd() *cobra.Command {
	return simpleAgentCmd("stop", "Stop an agent", "", (*DaemonClient).StopAgent, "stopped")
}

func agentsDestroyCmd() *cobra.Command {
	return simpleAgentCmd("destroy", "Destroy an agent and clean up all artifacts", "", (*DaemonClient).DestroyAgent, "destroyed")
}

func agentsRestartCmd() *cobra.Command {
	return simpleAgentCmd("restart", "Restart an agent (resets restart counter)", "", (*DaemonClient).RestartAgent, "restarted")
}

func agentsLogsCmd() *cobra.Command {
	var (
		follow bool
		since  string
		tail   int
	)

	cmd := &cobra.Command{
		Use:   "logs AGENT_ID",
		Short: "Show logs for an agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]

			if err := types.ValidatePathComponent("agent_id", agentID); err != nil {
				return err
			}

			absRoot, err := filepath.Abs(clusterRoot)
			if err != nil {
				return fmt.Errorf("resolving cluster root: %w", err)
			}

			logDir := filepath.Join(absRoot, ".state", "logs", agentID)
			if _, err := os.Stat(logDir); os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "No logs found for agent %q\n", agentID)
				return nil
			}

			entries, err := os.ReadDir(logDir)
			if err != nil {
				return fmt.Errorf("reading log directory: %w", err)
			}

			// Parse --since duration to compute a cutoff time.
			var sinceTime time.Time
			if since != "" {
				dur, err := time.ParseDuration(since)
				if err != nil {
					return fmt.Errorf("invalid --since duration %q: %w", since, err)
				}
				sinceTime = time.Now().Add(-dur)
			}

			// Collect all JSONL files sorted by name (chronological).
			var logFiles []string
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				if filepath.Ext(e.Name()) == extJSONL {
					logFiles = append(logFiles, filepath.Join(logDir, e.Name()))
				}
			}

			// Read and filter log lines, keeping at most --tail lines.
			// maxLogFileSize is the maximum file size we read entirely into memory.
			// For files exceeding this limit, only the last maxLogFileSize bytes are read.
			const maxLogFileSize = 10 * 1024 * 1024 // 10MB
			var lines []string
			for _, f := range logFiles {
				data, err := readLogFile(f, maxLogFileSize)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Warning: could not read %s: %v\n", f, err)
					continue
				}
				for _, line := range splitLines(string(data)) {
					if line == "" {
						continue
					}
					if !sinceTime.IsZero() {
						// Best-effort timestamp filter: parse the timestamp field.
						var entry struct {
							Timestamp time.Time `json:"timestamp"`
						}
						if err := json.Unmarshal([]byte(line), &entry); err == nil {
							if entry.Timestamp.Before(sinceTime) {
								continue
							}
						}
					}
					lines = append(lines, line)
				}
			}

			// Apply --tail limit (from the end).
			if tail > 0 && len(lines) > tail {
				lines = lines[len(lines)-tail:]
			}

			for _, line := range lines {
				fmt.Println(line)
			}

			if follow {
				fmt.Fprintf(os.Stderr, "Following logs for %s (press Ctrl+C to stop)...\n", agentID)
				return followLogs(cmd.Context(), logDir, sinceTime)
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&follow, "follow", false, "Follow log output")
	cmd.Flags().StringVar(&since, "since", "", "Show logs since duration (e.g., 5m, 1h)")
	cmd.Flags().IntVar(&tail, "tail", 100, "Number of recent log lines to show")

	return cmd
}

// splitLines splits a string into lines, handling both \n and \r\n.
func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			line := s[start:i]
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			lines = append(lines, line)
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// readLogFile reads a log file, returning its contents. If the file exceeds
// maxSize bytes, only the last maxSize bytes are returned so that the most
// recent log entries are preserved.
func readLogFile(path string, maxSize int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	if info.Size() <= maxSize {
		return os.ReadFile(path)
	}

	// File exceeds the limit; read only the last maxSize bytes.
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if _, err := f.Seek(-maxSize, io.SeekEnd); err != nil {
		return nil, err
	}

	data, err := io.ReadAll(io.LimitReader(f, maxSize))
	if err != nil {
		return nil, err
	}

	// Skip partial first line (since we seeked into the middle of the file).
	if idx := indexOf(data, '\n'); idx >= 0 && idx < len(data)-1 {
		data = data[idx+1:]
	}
	return data, nil
}

// indexOf returns the index of the first occurrence of b in data, or -1.
func indexOf(data []byte, b byte) int {
	for i, v := range data {
		if v == b {
			return i
		}
	}
	return -1
}

// maxFollowReadSize is the maximum number of bytes read per file per poll
// cycle in followLogs, preventing unbounded memory allocation if a log file
// grows very quickly between polls.
const maxFollowReadSize = 10 * 1024 * 1024 // 10MB

// followLogs watches the log directory for new JSONL content using polling.
// It handles SIGINT and SIGTERM for clean shutdown instead of requiring a
// hard kill.
func followLogs(parent context.Context, logDir string, sinceTime time.Time) error {
	// Set up signal handling with context cancellation.
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		<-sigCh
		cancel()
	}()

	// Track file sizes to detect new content.
	offsets := make(map[string]int64)

	// Initialize offsets to current end-of-file.
	entries, _ := os.ReadDir(logDir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == extJSONL {
			path := filepath.Join(logDir, e.Name())
			info, err := os.Stat(path)
			if err == nil {
				offsets[path] = info.Size()
			}
		}
	}

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}

		entries, err := os.ReadDir(logDir)
		if err != nil {
			continue
		}

		for _, e := range entries {
			if filepath.Ext(e.Name()) != extJSONL {
				continue
			}
			path := filepath.Join(logDir, e.Name())
			info, err := os.Stat(path)
			if err != nil {
				continue
			}

			prevOffset := offsets[path]
			if info.Size() < offsets[path] {
				offsets[path] = 0 // file was truncated (log rotation)
				prevOffset = 0
			}
			if info.Size() <= prevOffset {
				continue
			}

			readSize := info.Size() - prevOffset
			seekOffset := prevOffset
			if readSize > maxFollowReadSize {
				// Cap the read to avoid unbounded allocation; skip
				// ahead so we read the most recent bytes.
				seekOffset = info.Size() - maxFollowReadSize
				readSize = maxFollowReadSize
			}

			f, err := os.Open(path)
			if err != nil {
				continue
			}
			if _, err := f.Seek(seekOffset, 0); err != nil {
				f.Close()
				continue
			}

			buf := make([]byte, readSize)
			n, err := f.Read(buf)
			f.Close()
			if err != nil && !errors.Is(err, io.EOF) {
				continue
			}

			offsets[path] = seekOffset + int64(n)

			for _, line := range splitLines(string(buf[:n])) {
				if line == "" {
					continue
				}
				fmt.Println(line)
			}
		}
	}
}

func agentsExecCmd() *cobra.Command {
	var timeout int

	cmd := &cobra.Command{
		Use:   "exec AGENT_ID -- COMMAND [ARGS...]",
		Short: "Execute a command inside an agent's runtime",
		Long: `Execute a command inside an agent's sidecar runtime via NATS.

The command is sent to the agent's sidecar control handler on the NATS
subject hive.agent.{AGENT_ID}.sidecar.exec, which executes it in the
agent's runtime environment. Stdout and stderr are streamed back via
NATS reply.

Example:
  hivectl agents exec my-agent -- ls -la /workspace
  hivectl agents exec my-agent -- python3 -c "print('hello')"`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]

			if err := types.ValidateSubjectComponent("agent_id", agentID); err != nil {
				return err
			}

			// Extract the command after "--".
			var execArgs []string
			dashDash := cmd.ArgsLenAtDash()
			if dashDash >= 0 && dashDash < len(args) {
				execArgs = args[dashDash:]
			} else if len(args) > 1 {
				execArgs = args[1:]
			}

			if len(execArgs) == 0 {
				return fmt.Errorf("no command specified; use -- to separate the command from exec flags")
			}

			// Verify agent is running.
			var agentState *state.AgentState
			if err := withClient(func(client *DaemonClient) error {
				var statusErr error
				agentState, statusErr = client.AgentStatus(agentID)
				if statusErr != nil {
					return fmt.Errorf("checking agent status: %w", statusErr)
				}
				return nil
			}); err != nil {
				return err
			}

			if agentState.Status != state.AgentStatusRunning {
				return fmt.Errorf("agent %q is not running (status: %s)", agentID, agentState.Status)
			}

			// Connect to NATS for the exec request.
			nc, err := connectNATS(fmt.Sprintf("hivectl-exec-%s", agentID))
			if err != nil {
				return err
			}
			defer func() { _ = nc.Drain() }()

			// Build exec request payload.
			payloadData, err := json.Marshal(map[string]interface{}{
				"action":  "exec",
				"command": execArgs,
			})
			if err != nil {
				return fmt.Errorf("marshaling exec payload: %w", err)
			}

			execReq := types.Envelope{
				ID:        types.NewUUID(),
				From:      "hivectl",
				To:        agentID,
				Type:      types.MessageTypeControl,
				Timestamp: time.Now().UTC(),
				Payload:   payloadData,
				UserToken: authToken,
			}

			envData, err := json.Marshal(execReq)
			if err != nil {
				return fmt.Errorf("marshaling exec request: %w", err)
			}

			// Send the exec request to the sidecar's control handler.
			subject := fmt.Sprintf(protocol.FmtAgentSidecarExec, agentID)
			resp, err := nc.Request(subject, envData, time.Duration(timeout)*time.Second)
			if err != nil {
				if errors.Is(err, nats.ErrTimeout) {
					return fmt.Errorf("exec timed out after %ds: agent sidecar may not support exec or is unresponsive", timeout)
				}
				return fmt.Errorf("exec request failed: %w", err)
			}

			// Parse the response.
			var execResp struct {
				ExitCode int    `json:"exit_code"`
				Stdout   string `json:"stdout"`
				Stderr   string `json:"stderr"`
				Error    string `json:"error,omitempty"`
			}
			if err := json.Unmarshal(resp.Data, &execResp); err != nil {
				// If we can't parse as structured response, print raw output.
				fmt.Print(string(resp.Data))
				return nil
			}

			if execResp.Error != "" {
				return fmt.Errorf("exec error: %s", execResp.Error)
			}

			if execResp.Stdout != "" {
				fmt.Print(execResp.Stdout)
			}
			if execResp.Stderr != "" {
				fmt.Fprint(os.Stderr, execResp.Stderr)
			}

			if execResp.ExitCode != 0 {
				return fmt.Errorf("command exited with code %d", execResp.ExitCode)
			}

			return nil
		},
	}

	cmd.Flags().IntVar(&timeout, "timeout", 30, "Exec timeout in seconds")

	return cmd
}

func agentsCapabilitiesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "capabilities AGENT_ID",
		Short: "List capabilities for an agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]

			if err := types.ValidateSubjectComponent("agent_id", agentID); err != nil {
				return err
			}

			absRoot, err := filepath.Abs(clusterRoot)
			if err != nil {
				return fmt.Errorf("resolving cluster root: %w", err)
			}

			agents, err := config.LoadAgents(absRoot)
			if err != nil {
				return fmt.Errorf("loading agents: %w", err)
			}

			agent, ok := agents[agentID]
			if !ok {
				return fmt.Errorf("agent %q not found in manifests", agentID)
			}

			if len(agent.Spec.Capabilities) == 0 {
				fmt.Printf("Agent %s has no capabilities defined\n", agentID)
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tDESCRIPTION\tASYNC\tINPUTS\tOUTPUTS")

			for _, cap := range agent.Spec.Capabilities {
				inputs := capParamSummary(cap.Inputs)
				outputs := capParamSummary(cap.Outputs)
				async := "false"
				if cap.Async {
					async = "true"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
					cap.Name,
					cap.Description,
					async,
					inputs,
					outputs,
				)
			}

			w.Flush()
			return nil
		},
	}
}

// capParamSummary returns a comma-separated list of "name:type" strings, or "-" if empty.
func capParamSummary(params []types.CapabilityParam) string {
	if len(params) == 0 {
		return "-"
	}
	parts := make([]string, len(params))
	for i, p := range params {
		parts[i] = p.Name + ":" + p.Type
	}
	return strings.Join(parts, ", ")
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}

func agentsAddCmd() *cobra.Command {
	var (
		team        string
		runtimeType string
	)

	cmd := &cobra.Command{
		Use:   "add AGENT_ID",
		Short: "Add a new agent to an existing cluster",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]

			if err := types.ValidatePathComponent("agent_id", agentID); err != nil {
				return err
			}

			absRoot, err := filepath.Abs(clusterRoot)
			if err != nil {
				return fmt.Errorf("resolving cluster root: %w", err)
			}

			agentDir := filepath.Join(absRoot, "agents", agentID)
			if _, err := os.Stat(agentDir); err == nil {
				return fmt.Errorf("agent %q already exists at %s", agentID, agentDir)
			}

			scanner := bufio.NewScanner(os.Stdin)

			// Determine runtime type.
			rt := runtimeType
			if rt == "" {
				rt = runtimeOpenclaw
			}

			type addAgentValues struct {
				AgentID        string
				TeamID         string
				RuntimeType    string
				RuntimeCommand string
				OpenRouterKey  string
				TelegramToken  string
				TelegramUserID string
				Model          string
			}

			vals := addAgentValues{
				AgentID:     agentID,
				RuntimeType: rt,
			}

			teamID := team
			if teamID == "" {
				teamID = "default"
			}
			vals.TeamID = teamID

			// Runtime-specific prompts.
			if rt == runtimeOpenclaw {
				// Check if env file already has the API key.
				envPath := filepath.Join(absRoot, ".secrets", "env")
				existingKey := readEnvFileKey(envPath, "OPENROUTER_API_KEY")
				if existingKey != "" {
					vals.OpenRouterKey = existingKey
					fmt.Println("OpenRouter API key: (using existing key from .secrets/env)")
				} else {
					key, err := promptRequired(scanner, "OpenRouter API key")
					if err != nil {
						return err
					}
					vals.OpenRouterKey = key
					// Write env file for envsubst.
					secretsDir := filepath.Join(absRoot, ".secrets")
					if err := os.MkdirAll(secretsDir, 0700); err == nil {
						envContent := "OPENROUTER_API_KEY=" + key + "\n"
						_ = os.WriteFile(envPath, []byte(envContent), 0600)
					}
				}

				fmt.Print("Enable Telegram? [y/N]: ")
				if !scanner.Scan() {
					if err := scanner.Err(); err != nil {
						return fmt.Errorf("reading input: %w", err)
					}
					return fmt.Errorf("no input received")
				}
				enableTelegram := strings.TrimSpace(strings.ToLower(scanner.Text()))
				if enableTelegram == "y" || enableTelegram == "yes" {
					token, err := promptRequired(scanner, "Telegram bot token")
					if err != nil {
						return err
					}
					vals.TelegramToken = token
					userID, err := promptRequired(scanner, "Telegram user ID")
					if err != nil {
						return err
					}
					vals.TelegramUserID = userID
				}

				vals.Model = promptWithDefault(scanner, "Model", "openrouter/moonshotai/kimi-k2.5")
			} else if rt == runtimeCustom {
				command, err := promptRequired(scanner, "Runtime command (full path)")
				if err != nil {
					return err
				}
				vals.RuntimeCommand = command
			}

			// Create agent directory.
			if err := os.MkdirAll(agentDir, 0755); err != nil {
				return fmt.Errorf("creating agent dir: %w", err)
			}

			reg := templates.DefaultRegistry()

			// Manifest.
			manifestPath := filepath.Join(agentDir, "manifest.yaml")
			manifestContent, err := reg.Render("init/add-agent-manifest.yaml.tmpl", vals)
			if err != nil {
				return fmt.Errorf("rendering manifest: %w", err)
			}
			if err := os.WriteFile(manifestPath, manifestContent, 0644); err != nil {
				return fmt.Errorf("writing manifest: %w", err)
			}

			fmt.Printf("Agent %s created at %s\n", agentID, agentDir)
			fmt.Printf("  manifest:  %s\n", manifestPath)

			// Runtime config (OpenClaw only).
			if rt == runtimeOpenclaw {
				openclawPath := filepath.Join(agentDir, "openclaw.json")
				openclawContent, err := reg.Render("init/runtime-config.json.tmpl", vals)
				if err != nil {
					return fmt.Errorf("rendering runtime config: %w", err)
				}
				if err := os.WriteFile(openclawPath, openclawContent, 0600); err != nil {
					return fmt.Errorf("writing runtime config: %w", err)
				}
				fmt.Printf("  config:    %s\n", openclawPath)
			}

			// AGENTS.md
			agentsMdPath := filepath.Join(agentDir, "AGENTS.md")
			if err := os.WriteFile(agentsMdPath, []byte(fmt.Sprintf("# %s\n\n(Define this agent's identity and instructions here.)\n", agentID)), 0644); err != nil {
				return fmt.Errorf("writing AGENTS.md: %w", err)
			}

			// MEMORY.md
			memoryMdPath := filepath.Join(agentDir, "MEMORY.md")
			if err := os.WriteFile(memoryMdPath, []byte("# Memory\n\n(This file is automatically synced to the agent's workspace. Add persistent notes here.)\n"), 0644); err != nil {
				return fmt.Errorf("writing MEMORY.md: %w", err)
			}

			fmt.Printf("  AGENTS.md: %s\n", agentsMdPath)

			// Update team manifest if --team specified.
			if team != "" {
				teamDir := filepath.Join(absRoot, "teams")
				teamFile := filepath.Join(teamDir, team+".yaml")
				if _, err := os.Stat(teamFile); err == nil {
					fmt.Printf("  team: agent's manifest.yaml has metadata.team set to %q\n", team)
					fmt.Printf("         Team membership is determined by the agent's metadata.team field.\n")
				} else {
					fmt.Printf("  warning: team %q does not exist yet; create %s\n", team, teamFile)
				}
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&team, "team", "", "Team to add the agent to")
	cmd.Flags().StringVar(&runtimeType, "runtime", "", "Runtime type: openclaw (default), custom")

	return cmd
}
