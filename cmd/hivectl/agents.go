package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/hivehq/hive/internal/config"
	"github.com/hivehq/hive/internal/state"
	"github.com/hivehq/hive/internal/types"
	"github.com/hivehq/hive/internal/vm"
	"github.com/spf13/cobra"
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

	return cmd
}

func newManager() (*vm.Manager, *state.Store, error) {
	absRoot, err := filepath.Abs(clusterRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("resolving cluster root: %w", err)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	statePath := filepath.Join(absRoot, "state.json")
	store, err := state.NewStore(statePath, logger)
	if err != nil {
		return nil, nil, fmt.Errorf("opening state store: %w", err)
	}

	var hyp vm.Hypervisor
	if os.Getenv("HIVE_TEST_FIRECRACKER") == "mock" {
		hyp = vm.NewMockHypervisor()
	} else {
		hyp = vm.NewFirecrackerHypervisor("firecracker", logger)
	}

	mgr := vm.NewManager(absRoot, store, logger, hyp)
	return mgr, store, nil
}

func agentsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all agents with their current state",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, store, err := newManager()
			if err != nil {
				return err
			}

			agents := store.AllAgents()

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
		},
	}
}

func agentsStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status AGENT_ID",
		Short: "Show detailed status for an agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, store, err := newManager()
			if err != nil {
				return err
			}

			agentID := args[0]
			a := store.GetAgent(agentID)
			if a == nil {
				fmt.Fprintf(os.Stderr, "Error: agent %q not found\n", agentID)
				os.Exit(1)
			}

			data, err := json.MarshalIndent(a, "", "  ")
			if err != nil {
				return fmt.Errorf("marshaling agent state: %w", err)
			}
			fmt.Println(string(data))
			return nil
		},
	}
}

func agentsStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start AGENT_ID",
		Short: "Start an agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]

			absRoot, err := filepath.Abs(clusterRoot)
			if err != nil {
				return fmt.Errorf("resolving cluster root: %w", err)
			}

			// Load agent manifest
			agents, err := config.LoadAgents(absRoot)
			if err != nil {
				return fmt.Errorf("loading agents: %w", err)
			}

			agent, ok := agents[agentID]
			if !ok {
				fmt.Fprintf(os.Stderr, "Error: agent %q not found in manifests\n", agentID)
				os.Exit(1)
			}

			mgr, _, err := newManager()
			if err != nil {
				return err
			}

			if err := mgr.StartAgent(agent); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}

			fmt.Printf("Agent %s started\n", agentID)
			return nil
		},
	}
}

func agentsStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop AGENT_ID",
		Short: "Stop an agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]

			mgr, _, err := newManager()
			if err != nil {
				return err
			}

			if err := mgr.StopAgent(agentID); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}

			fmt.Printf("Agent %s stopped\n", agentID)
			return nil
		},
	}
}

func agentsDestroyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "destroy AGENT_ID",
		Short: "Destroy an agent and clean up all artifacts",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]

			mgr, _, err := newManager()
			if err != nil {
				return err
			}

			if err := mgr.DestroyAgent(agentID); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}

			fmt.Printf("Agent %s destroyed\n", agentID)
			return nil
		},
	}
}

func agentsRestartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restart AGENT_ID",
		Short: "Restart an agent (resets restart counter)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]

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
				fmt.Fprintf(os.Stderr, "Error: agent %q not found in manifests\n", agentID)
				os.Exit(1)
			}

			mgr, _, err := newManager()
			if err != nil {
				return err
			}

			if err := mgr.RestartAgent(agentID, agent); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}

			fmt.Printf("Agent %s restarted\n", agentID)
			return nil
		},
	}
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
				if filepath.Ext(e.Name()) == ".jsonl" {
					logFiles = append(logFiles, filepath.Join(logDir, e.Name()))
				}
			}

			// Read and filter log lines, keeping at most --tail lines.
			var lines []string
			for _, f := range logFiles {
				data, err := os.ReadFile(f)
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
				return followLogs(logDir, sinceTime)
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

// followLogs watches the log directory for new JSONL content using polling.
func followLogs(logDir string, sinceTime time.Time) error {
	// Track file sizes to detect new content.
	offsets := make(map[string]int64)

	// Initialize offsets to current end-of-file.
	entries, _ := os.ReadDir(logDir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".jsonl" {
			path := filepath.Join(logDir, e.Name())
			info, err := os.Stat(path)
			if err == nil {
				offsets[path] = info.Size()
			}
		}
	}

	for {
		time.Sleep(1 * time.Second)

		entries, err := os.ReadDir(logDir)
		if err != nil {
			continue
		}

		for _, e := range entries {
			if filepath.Ext(e.Name()) != ".jsonl" {
				continue
			}
			path := filepath.Join(logDir, e.Name())
			info, err := os.Stat(path)
			if err != nil {
				continue
			}

			prevOffset := offsets[path]
			if info.Size() <= prevOffset {
				continue
			}

			f, err := os.Open(path)
			if err != nil {
				continue
			}
			if _, err := f.Seek(prevOffset, 0); err != nil {
				f.Close()
				continue
			}

			buf := make([]byte, info.Size()-prevOffset)
			n, _ := f.Read(buf)
			f.Close()

			offsets[path] = prevOffset + int64(n)

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
	return &cobra.Command{
		Use:   "exec AGENT_ID -- COMMAND",
		Short: "Execute a command inside an agent's runtime",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("exec not available: requires running hived with VM access")
			return nil
		},
	}
}

func agentsCapabilitiesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "capabilities AGENT_ID",
		Short: "List capabilities for an agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]

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
				fmt.Fprintf(os.Stderr, "Error: agent %q not found in manifests\n", agentID)
				os.Exit(1)
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
