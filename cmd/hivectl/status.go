// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/brmurrell3/hive/internal/protocol"
	"github.com/brmurrell3/hive/internal/state"
	"github.com/spf13/cobra"
)

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show cluster overview",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(func(client *DaemonClient) error {
				resp, err := client.request(protocol.SubjStatus, nil)
				if err != nil {
					return fmt.Errorf("requesting cluster status: %w", err)
				}
				if err := resp.Err(); err != nil {
					return err
				}

				var status struct {
					ClusterName  string         `json:"cluster_name"`
					NodeCount    int            `json:"node_count"`
					TeamCount    int            `json:"team_count"`
					AgentCount   int            `json:"agent_count"`
					StatusCounts map[string]int `json:"status_counts,omitempty"`
					NATSPort     int            `json:"nats_port"`
				}
				if err := json.Unmarshal(resp.Data, &status); err != nil {
					return fmt.Errorf("parsing response: %w", err)
				}

				natsStatus := "configured"
				if status.NATSPort > 0 {
					natsStatus = fmt.Sprintf("configured (port %d)", status.NATSPort)
				}

				w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
				fmt.Fprintf(w, "Cluster:\t%s\n", status.ClusterName)
				fmt.Fprintf(w, "Nodes:\t%d\n", status.NodeCount)
				fmt.Fprintf(w, "Teams:\t%d\n", status.TeamCount)
				fmt.Fprintf(w, "Agents:\t%d\n", status.AgentCount)

				if status.AgentCount > 0 {
					for _, s := range []state.AgentStatus{
						state.AgentStatusRunning,
						state.AgentStatusStopped,
						state.AgentStatusFailed,
						state.AgentStatusPending,
						state.AgentStatusCreating,
						state.AgentStatusStarting,
						state.AgentStatusStopping,
					} {
						if count, ok := status.StatusCounts[string(s)]; ok && count > 0 {
							coloredStatus := colorizeAgentStatus(s, 0)
							fmt.Fprintf(w, "  %s:\t%d\n", coloredStatus, count)
						}
					}
				}

				fmt.Fprintf(w, "NATS:\t%s\n", natsStatus)
				w.Flush()

				return nil
			})
		},
	}
}
