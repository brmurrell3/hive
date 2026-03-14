// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/brmurrell3/hive/internal/protocol"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/spf13/cobra"
)

func nodesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "nodes",
		Short: "Manage cluster nodes",
	}

	cmd.AddCommand(nodesListCmd())
	cmd.AddCommand(nodesStatusCmd())
	cmd.AddCommand(nodesDrainCmd())
	cmd.AddCommand(nodesCordonCmd())
	cmd.AddCommand(nodesUncordonCmd())
	cmd.AddCommand(nodesLabelCmd())
	cmd.AddCommand(nodesUnlabelCmd())
	cmd.AddCommand(nodesApproveCmd())
	cmd.AddCommand(nodesRemoveCmd())

	return cmd
}

func nodesListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all registered nodes",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(func(client *DaemonClient) error {
				resp, err := client.request(protocol.SubjNodeList, nil)
				if err != nil {
					return fmt.Errorf("listing nodes: %w", err)
				}
				if err := resp.Err(); err != nil {
					return err
				}

				var nodes []*types.NodeState
				if err := json.Unmarshal(resp.Data, &nodes); err != nil {
					return fmt.Errorf("parsing response: %w", err)
				}

				w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
				fmt.Fprintln(w, "NODE_ID\tTIER\tARCH\tSTATUS\tMEMORY\tCPUS\tAGENTS")

				for _, n := range nodes {
					memStr := formatBytes(n.Resources.MemoryTotal)
					agentCount := len(n.Agents)
					fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%d\t%d\n",
						n.ID,
						n.Tier,
						n.Arch,
						n.Status,
						memStr,
						n.Resources.CPUCount,
						agentCount,
					)
				}

				w.Flush()
				return nil
			})
		},
	}
}

func nodesStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status NODE_ID",
		Short: "Show detailed status for a node",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			nodeID := args[0]

			if err := types.ValidateSubjectComponent("node_id", nodeID); err != nil {
				return err
			}

			return withClient(func(client *DaemonClient) error {
				resp, err := client.request(protocol.SubjNodeStatus, protocol.CtlRequest{AgentID: nodeID})
				if err != nil {
					return fmt.Errorf("getting node status: %w", err)
				}
				if err := resp.Err(); err != nil {
					return err
				}

				var n types.NodeState
				if err := json.Unmarshal(resp.Data, &n); err != nil {
					return fmt.Errorf("parsing response: %w", err)
				}

				data, err := json.MarshalIndent(n, "", "  ")
				if err != nil {
					return fmt.Errorf("marshaling node state: %w", err)
				}
				fmt.Println(string(data))
				return nil
			})
		},
	}
}

// simpleNodeCmd creates a cobra command that validates a node ID,
// sends a node control request, and prints a success message.
func simpleNodeCmd(use, short string, subject, pastMsg string) *cobra.Command {
	return simpleResourceCmd(use+" NODE_ID", short, "", "node_id", "Node", func(client *DaemonClient, nodeID string) error {
		resp, err := client.request(subject, protocol.CtlRequest{AgentID: nodeID})
		if err != nil {
			return err
		}
		return resp.Err()
	}, pastMsg)
}

func nodesDrainCmd() *cobra.Command {
	return simpleNodeCmd("drain", "Drain a node by marking it as draining (prevents new scheduling, signals agent migration)",
		protocol.SubjNodeDrain, "marked as draining")
}

func nodesCordonCmd() *cobra.Command {
	return simpleNodeCmd("cordon", "Cordon a node to prevent new agent scheduling",
		protocol.SubjNodeCordon, "cordoned")
}

func nodesUncordonCmd() *cobra.Command {
	return simpleNodeCmd("uncordon", "Uncordon a node to allow new agent scheduling",
		protocol.SubjNodeUncordon, "uncordoned (now online)")
}

func nodesLabelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "label NODE_ID KEY=VALUE [KEY=VALUE ...]",
		Short: "Add labels to a node",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			nodeID := args[0]

			if err := types.ValidateSubjectComponent("node_id", nodeID); err != nil {
				return err
			}

			labels := make(map[string]string)

			for _, kv := range args[1:] {
				parts := strings.SplitN(kv, "=", 2)
				if len(parts) != 2 {
					return fmt.Errorf("invalid label format %q: expected KEY=VALUE", kv)
				}
				key, value := parts[0], parts[1]
				if key == "" {
					return fmt.Errorf("label key cannot be empty in %q", kv)
				}
				labels[key] = value
			}

			return withClient(func(client *DaemonClient) error {
				resp, err := client.request(protocol.SubjNodeLabel, map[string]interface{}{
					"node_id": nodeID,
					"labels":  labels,
				})
				if err != nil {
					return fmt.Errorf("labeling node: %w", err)
				}
				if err := resp.Err(); err != nil {
					return err
				}

				fmt.Printf("Node %s labeled\n", nodeID)
				return nil
			})
		},
	}
}

func nodesUnlabelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unlabel NODE_ID KEY [KEY ...]",
		Short: "Remove labels from a node",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			nodeID := args[0]

			if err := types.ValidateSubjectComponent("node_id", nodeID); err != nil {
				return err
			}

			keys := args[1:]

			return withClient(func(client *DaemonClient) error {
				resp, err := client.request(protocol.SubjNodeUnlabel, map[string]interface{}{
					"node_id": nodeID,
					"keys":    keys,
				})
				if err != nil {
					return fmt.Errorf("unlabeling node: %w", err)
				}
				if err := resp.Err(); err != nil {
					return err
				}

				fmt.Printf("Node %s unlabeled\n", nodeID)
				return nil
			})
		},
	}
}

func nodesApproveCmd() *cobra.Command {
	return simpleNodeCmd("approve", "Approve a pending node and set its status to online",
		protocol.SubjNodeApprove, "approved (now online)")
}

func nodesRemoveCmd() *cobra.Command {
	return simpleNodeCmd("remove", "Remove a node from the cluster",
		protocol.SubjNodeRemove, "removed")
}

func formatBytes(b int64) string {
	if b == 0 {
		return "-"
	}

	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
	)

	switch {
	case b >= gb:
		return fmt.Sprintf("%.1fGi", float64(b)/float64(gb))
	case b >= mb:
		return fmt.Sprintf("%.0fMi", float64(b)/float64(mb))
	case b >= kb:
		return fmt.Sprintf("%.0fKi", float64(b)/float64(kb))
	default:
		return fmt.Sprintf("%dB", b)
	}
}
