// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/charmbracelet/lipgloss"
)

// CLI color styles using lipgloss for consistent terminal output.
var (
	// Agent status colors.
	styleStatusRunning  = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))  // green
	styleStatusStopped  = lipgloss.NewStyle().Foreground(lipgloss.Color("243")) // gray
	styleStatusFailed   = lipgloss.NewStyle().Foreground(lipgloss.Color("196")) // red
	styleStatusPending  = lipgloss.NewStyle().Foreground(lipgloss.Color("214")) // yellow/orange
	styleStatusCreating = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))  // blue
	styleStatusStarting = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))  // blue
	styleStatusStopping = lipgloss.NewStyle().Foreground(lipgloss.Color("214")) // yellow/orange

	// Node status colors.
	styleNodeOnline   = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))  // green
	styleNodeOffline  = lipgloss.NewStyle().Foreground(lipgloss.Color("196")) // red
	styleNodeDraining = lipgloss.NewStyle().Foreground(lipgloss.Color("214")) // yellow/orange
	styleNodeCordoned = lipgloss.NewStyle().Foreground(lipgloss.Color("243")) // gray
	styleNodePending  = lipgloss.NewStyle().Foreground(lipgloss.Color("214")) // yellow/orange

	// Doctor check status colors.
	styleCheckPASS = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))  // green
	styleCheckFAIL = lipgloss.NewStyle().Foreground(lipgloss.Color("196")) // red
	styleCheckWARN = lipgloss.NewStyle().Foreground(lipgloss.Color("214")) // yellow/orange
	styleCheckSKIP = lipgloss.NewStyle().Foreground(lipgloss.Color("243")) // gray

	// Header style for table headers.
	styleHeader = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("255"))

	// Log level colors.
	styleLogDebug = lipgloss.NewStyle().Foreground(lipgloss.Color("243")) // gray
	styleLogInfo  = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))  // blue
	styleLogWarn  = lipgloss.NewStyle().Foreground(lipgloss.Color("214")) // yellow/orange
	styleLogError = lipgloss.NewStyle().Foreground(lipgloss.Color("196")) // red

	// Timestamp style for log output.
	styleTimestamp = lipgloss.NewStyle().Foreground(lipgloss.Color("243")) // gray

	// Agent ID style for log output.
	styleAgentID = lipgloss.NewStyle().Foreground(lipgloss.Color("39")) // blue
)

// colorizeAgentStatus returns a colorized string for an agent status,
// padded to the given width for table alignment.
func colorizeAgentStatus(status state.AgentStatus, width int) string {
	s := string(status)
	var styled string

	switch status {
	case state.AgentStatusRunning:
		styled = styleStatusRunning.Render(s)
	case state.AgentStatusStopped:
		styled = styleStatusStopped.Render(s)
	case state.AgentStatusFailed:
		styled = styleStatusFailed.Render(s)
	case state.AgentStatusPending:
		styled = styleStatusPending.Render(s)
	case state.AgentStatusCreating:
		styled = styleStatusCreating.Render(s)
	case state.AgentStatusStarting:
		styled = styleStatusStarting.Render(s)
	case state.AgentStatusStopping:
		styled = styleStatusStopping.Render(s)
	default:
		styled = s
	}

	return padStyled(styled, s, width)
}

// colorizeNodeStatus returns a colorized string for a node status,
// padded to the given width for table alignment.
func colorizeNodeStatus(status types.NodeStatus, width int) string {
	s := string(status)
	var styled string

	switch status {
	case types.NodeStatusOnline:
		styled = styleNodeOnline.Render(s)
	case types.NodeStatusOffline:
		styled = styleNodeOffline.Render(s)
	case types.NodeStatusDraining:
		styled = styleNodeDraining.Render(s)
	case types.NodeStatusCordoned:
		styled = styleNodeCordoned.Render(s)
	case types.NodeStatusPending:
		styled = styleNodePending.Render(s)
	default:
		styled = s
	}

	return padStyled(styled, s, width)
}

// colorizeCheckStatus returns a colorized string for a doctor check status.
func colorizeCheckStatus(status checkStatus) string {
	s := status.String()
	switch status {
	case checkPASS:
		return styleCheckPASS.Render(s)
	case checkFAIL:
		return styleCheckFAIL.Render(s)
	case checkWARN:
		return styleCheckWARN.Render(s)
	case checkSKIP:
		return styleCheckSKIP.Render(s)
	default:
		return s
	}
}

// colorizeLogLevel returns a colorized string for a log level.
func colorizeLogLevel(level string) string {
	switch strings.ToLower(level) {
	case "debug":
		return styleLogDebug.Render(level)
	case "info":
		return styleLogInfo.Render(level)
	case "warn", "warning":
		return styleLogWarn.Render(level)
	case "error":
		return styleLogError.Render(level)
	default:
		return level
	}
}

// padStyled pads a styled (ANSI-colored) string so that its visual width
// matches the target width. The raw parameter is the unstyled text used
// to calculate the necessary padding.
func padStyled(styled, raw string, width int) string {
	visWidth := len(raw)
	if visWidth < width {
		styled += strings.Repeat(" ", width-visWidth)
	}
	return styled
}

// colorizedHeader returns a bold header string for table output.
func colorizedHeader(text string) string {
	return styleHeader.Render(text)
}

// printColorizedAgentTable prints a colorized agent table to stdout.
func printColorizedAgentTable(agents []*state.AgentState) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
		colorizedHeader("AGENT_ID"),
		colorizedHeader("TEAM"),
		colorizedHeader("STATE"),
		colorizedHeader("UPTIME"),
	)

	for _, a := range agents {
		uptime := ""
		if a.Status == state.AgentStatusRunning && !a.StartedAt.IsZero() {
			uptime = formatDuration(time.Since(a.StartedAt))
		}
		team := a.Team
		if team == "" {
			team = "-"
		}
		// Use colorized status with fixed width for alignment.
		statusStr := colorizeAgentStatus(a.Status, 10)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", a.ID, team, statusStr, uptime)
	}

	w.Flush()
}
