// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/brmurrell3/hive/internal/protocol"
	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/types"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
)

func dashboardCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "dashboard",
		Short: "Interactive TUI dashboard",
		RunE: func(cmd *cobra.Command, args []string) error {
			m := &dashboardModel{
				tab:    0,
				width:  80,
				height: 24,
			}

			p := tea.NewProgram(m, tea.WithAltScreen())

			defer func() {
				if m.nc != nil {
					if err := m.nc.Drain(); err != nil {
						slog.Warn("failed to drain NATS connection", "error", err)
					}
				}
			}()

			if _, err := p.Run(); err != nil {
				return fmt.Errorf("dashboard error: %w", err)
			}

			return nil
		},
	}
}

type dashboardModel struct {
	tab    int
	width  int
	height int

	// Connection
	nc *nats.Conn

	// Data
	agents     []*state.AgentState
	lastUpdate time.Time
	err        error
}

type tickMsg time.Time

// connectedMsg carries a new NATS connection back to the Update goroutine
// so that m.nc is only ever written from Update (no data race).
type connectedMsg struct{ nc *nats.Conn }

func (m *dashboardModel) Init() tea.Cmd {
	return tea.Batch(
		connectAndFetchCmd(m.nc),
		tickCmd(),
	)
}

func tickCmd() tea.Cmd {
	return tea.Tick(5*time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// connectAndFetchCmd returns a tea.Cmd that connects to NATS (if needed) and
// fetches data. The caller must capture nc from m.nc on the main goroutine
// and pass it here so the Cmd closure never reads m.nc from a background
// goroutine (avoiding a data race).
func connectAndFetchCmd(nc *nats.Conn) tea.Cmd {
	return func() tea.Msg {
		if nc == nil || !nc.IsConnected() {
			newNC, err := connectNATS("hivectl-dashboard")
			if err != nil {
				return errMsg{err}
			}
			// Send the connection back through a message so Update can
			// assign it to m.nc on the main goroutine (no data race).
			return connectedMsg{nc: newNC}
		}
		return fetchFromNATS(nc)
	}
}

// fetchDataCmd returns a tea.Cmd that fetches data from NATS. The caller must
// capture nc from m.nc on the main goroutine so the closure is race-free.
func fetchDataCmd(nc *nats.Conn) tea.Cmd {
	return func() tea.Msg {
		if nc == nil || !nc.IsConnected() {
			return connectAndFetchCmd(nc)()
		}
		return fetchFromNATS(nc)
	}
}

// fetchFromNATS performs the actual NATS requests. It is a package-level
// function that receives the connection as a parameter so it never reads
// or writes model fields from the Cmd goroutine.
func fetchFromNATS(nc *nats.Conn) tea.Msg {
	if nc == nil {
		return errMsg{fmt.Errorf("not connected to NATS")}
	}

	// Fetch agents.
	ctlPayload, _ := json.Marshal(protocol.CtlRequest{})
	agentEnv := types.Envelope{
		ID:        types.NewUUID(),
		From:      "dashboard",
		To:        "hived",
		Type:      types.MessageTypeControl,
		Timestamp: time.Now().UTC(),
		Payload:   ctlPayload,
		UserToken: authToken,
	}
	agentEnvData, err := json.Marshal(agentEnv)
	if err != nil {
		return errMsg{fmt.Errorf("marshaling agent list request: %w", err)}
	}
	agentMsg, err := nc.Request(protocol.SubjAgentList, agentEnvData, defaultNATSTimeout)
	if err != nil {
		return errMsg{err}
	}

	var agentResp struct {
		Agents []*state.AgentState `json:"agents"`
	}
	if err := json.Unmarshal(agentMsg.Data, &agentResp); err != nil {
		return errMsg{fmt.Errorf("unmarshaling agent list response: %w", err)}
	}

	return dataMsg{agents: agentResp.Agents}
}

type dataMsg struct {
	agents []*state.AgentState
}

type errMsg struct{ err error }

func (m *dashboardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			// Do not drain m.nc here; the post-Run() cleanup in
			// dashboardCmd already calls m.nc.Drain() on all exit paths.
			return m, tea.Quit
		case "tab", "right":
			m.tab = (m.tab + 1) % 2
		case "shift+tab", "left":
			m.tab = (m.tab + 1) % 2
		case "r":
			return m, fetchDataCmd(m.nc)
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tickMsg:
		return m, tea.Batch(fetchDataCmd(m.nc), tickCmd())
	case connectedMsg:
		m.nc = msg.nc
		return m, fetchDataCmd(m.nc)
	case dataMsg:
		m.agents = msg.agents
		m.lastUpdate = time.Now()
		m.err = nil
	case errMsg:
		m.err = msg.err
	}
	return m, nil
}

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("205"))

	activeTabStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("39")).
			Underline(true)

	inactiveTabStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("244"))

	statusRunning = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	statusStopped = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	statusFailed  = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
)

func (m *dashboardModel) View() string {
	var b strings.Builder

	// Header
	b.WriteString(titleStyle.Render("Hive Dashboard"))
	b.WriteString("\n\n")

	// Tabs
	tabs := []string{"Agents", "Resources"}
	for i, tab := range tabs {
		if i == m.tab {
			b.WriteString(activeTabStyle.Render("[" + tab + "]"))
		} else {
			b.WriteString(inactiveTabStyle.Render(" " + tab + " "))
		}
		b.WriteString("  ")
	}
	b.WriteString("\n")
	b.WriteString(strings.Repeat("─", min(m.width, 80)))
	b.WriteString("\n\n")

	// Error
	if m.err != nil {
		b.WriteString(statusFailed.Render("Error: " + m.err.Error()))
		b.WriteString("\n\n")
	}

	// Content based on tab
	switch m.tab {
	case 0:
		b.WriteString(m.agentsView())
	case 1:
		b.WriteString(m.resourcesView())
	}

	// Footer
	b.WriteString("\n")
	b.WriteString(strings.Repeat("─", min(m.width, 80)))
	b.WriteString("\n")
	updated := "never"
	if !m.lastUpdate.IsZero() {
		updated = m.lastUpdate.Format("15:04:05")
	}
	b.WriteString(inactiveTabStyle.Render(
		fmt.Sprintf("Updated: %s | Tab: switch view | r: refresh | q: quit", updated)))
	b.WriteString("\n")

	return b.String()
}

func (m *dashboardModel) agentsView() string {
	if len(m.agents) == 0 {
		return "No agents running.\n"
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("%-20s %-12s %-10s %-10s\n", "AGENT", "STATUS", "TEAM", "NODE"))
	b.WriteString(strings.Repeat("─", 60))
	b.WriteString("\n")

	for _, a := range m.agents {
		status := colorStatus(string(a.Status), 12)
		team := a.Team
		if team == "" {
			team = "-"
		}
		nodeID := a.NodeID
		if nodeID == "" {
			nodeID = "-"
		}
		b.WriteString(fmt.Sprintf("%-20s %s %-10s %-10s\n", a.ID, status, team, nodeID))
	}

	b.WriteString(fmt.Sprintf("\nTotal: %d agents\n", len(m.agents)))
	return b.String()
}

func (m *dashboardModel) resourcesView() string {
	if len(m.agents) == 0 {
		return "No agents to show resources for.\n"
	}

	var b strings.Builder
	running := 0
	var totalMem int64
	var totalCPU int
	for _, a := range m.agents {
		if a.Status == state.AgentStatusRunning {
			running++
			totalMem += a.MemoryBytes / (1024 * 1024)
			totalCPU += a.VCPUs
		}
	}

	b.WriteString(fmt.Sprintf("Running agents:  %d\n", running))
	b.WriteString(fmt.Sprintf("Total memory:    %d MB\n", totalMem))
	b.WriteString(fmt.Sprintf("Total vCPUs:     %d\n", totalCPU))

	return b.String()
}

func colorStatus(status string, width int) string {
	var styled string
	switch status {
	case "running":
		styled = statusRunning.Render(status)
	case "stopped":
		styled = statusStopped.Render(status)
	case "failed":
		styled = statusFailed.Render(status)
	default:
		styled = status
	}
	// Pad with spaces to reach desired visual width.
	// lipgloss.Width returns the visual width excluding ANSI codes.
	visWidth := lipgloss.Width(styled)
	if visWidth < width {
		styled += strings.Repeat(" ", width-visWidth)
	}
	return styled
}
