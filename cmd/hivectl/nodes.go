package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

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
			store, err := newStoreOnly()
			if err != nil {
				return err
			}

			nodes := store.AllNodes()

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
		},
	}
}

func nodesStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status NODE_ID",
		Short: "Show detailed status for a node",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := newStoreOnly()
			if err != nil {
				return err
			}

			nodeID := args[0]
			n := store.GetNode(nodeID)
			if n == nil {
				return fmt.Errorf("node %q not found", nodeID)
			}

			data, err := json.MarshalIndent(n, "", "  ")
			if err != nil {
				return fmt.Errorf("marshaling node state: %w", err)
			}
			fmt.Println(string(data))
			return nil
		},
	}
}

func nodesDrainCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "drain NODE_ID",
		Short: "Drain a node by marking it as draining (prevents new scheduling, signals agent migration)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := newStoreOnly()
			if err != nil {
				return err
			}

			nodeID := args[0]
			n := store.GetNode(nodeID)
			if n == nil {
				return fmt.Errorf("node %q not found", nodeID)
			}

			if n.Status == types.NodeStatusDraining {
				fmt.Printf("Node %s is already draining\n", nodeID)
				return nil
			}

			n.Status = types.NodeStatusDraining
			if err := store.SetNode(n); err != nil {
				return fmt.Errorf("updating node state: %w", err)
			}

			fmt.Printf("Node %s marked as draining\n", nodeID)
			return nil
		},
	}
}

func nodesCordonCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cordon NODE_ID",
		Short: "Cordon a node to prevent new agent scheduling",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := newStoreOnly()
			if err != nil {
				return err
			}

			nodeID := args[0]
			n := store.GetNode(nodeID)
			if n == nil {
				return fmt.Errorf("node %q not found", nodeID)
			}

			if n.Status == types.NodeStatusCordoned {
				fmt.Printf("Node %s is already cordoned\n", nodeID)
				return nil
			}

			n.Status = types.NodeStatusCordoned
			if err := store.SetNode(n); err != nil {
				return fmt.Errorf("updating node state: %w", err)
			}

			fmt.Printf("Node %s cordoned\n", nodeID)
			return nil
		},
	}
}

func nodesUncordonCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uncordon NODE_ID",
		Short: "Uncordon a node to allow new agent scheduling",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := newStoreOnly()
			if err != nil {
				return err
			}

			nodeID := args[0]
			n := store.GetNode(nodeID)
			if n == nil {
				return fmt.Errorf("node %q not found", nodeID)
			}

			if n.Status != types.NodeStatusCordoned && n.Status != types.NodeStatusDraining {
				fmt.Printf("Node %s is not cordoned or draining (status: %s)\n", nodeID, n.Status)
				return nil
			}

			n.Status = types.NodeStatusOnline
			if err := store.SetNode(n); err != nil {
				return fmt.Errorf("updating node state: %w", err)
			}

			fmt.Printf("Node %s uncordoned (now online)\n", nodeID)
			return nil
		},
	}
}

func nodesLabelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "label NODE_ID KEY=VALUE [KEY=VALUE ...]",
		Short: "Add labels to a node",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := newStoreOnly()
			if err != nil {
				return err
			}

			nodeID := args[0]
			n := store.GetNode(nodeID)
			if n == nil {
				return fmt.Errorf("node %q not found", nodeID)
			}

			if n.Labels == nil {
				n.Labels = make(map[string]string)
			}

			for _, kv := range args[1:] {
				parts := strings.SplitN(kv, "=", 2)
				if len(parts) != 2 {
					return fmt.Errorf("invalid label format %q: expected KEY=VALUE", kv)
				}
				key, value := parts[0], parts[1]
				if key == "" {
					return fmt.Errorf("label key cannot be empty in %q", kv)
				}
				n.Labels[key] = value
			}

			if err := store.SetNode(n); err != nil {
				return fmt.Errorf("updating node state: %w", err)
			}

			fmt.Printf("Node %s labeled\n", nodeID)
			return nil
		},
	}
}

func nodesUnlabelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unlabel NODE_ID KEY [KEY ...]",
		Short: "Remove labels from a node",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := newStoreOnly()
			if err != nil {
				return err
			}

			nodeID := args[0]
			n := store.GetNode(nodeID)
			if n == nil {
				return fmt.Errorf("node %q not found", nodeID)
			}

			for _, key := range args[1:] {
				delete(n.Labels, key)
			}

			if err := store.SetNode(n); err != nil {
				return fmt.Errorf("updating node state: %w", err)
			}

			fmt.Printf("Node %s unlabeled\n", nodeID)
			return nil
		},
	}
}

func nodesApproveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "approve NODE_ID",
		Short: "Approve a pending node and set its status to online",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := newStoreOnly()
			if err != nil {
				return err
			}

			nodeID := args[0]
			n := store.GetNode(nodeID)
			if n == nil {
				return fmt.Errorf("node %q not found", nodeID)
			}

			n.Status = types.NodeStatusOnline
			if err := store.SetNode(n); err != nil {
				return fmt.Errorf("updating node state: %w", err)
			}

			fmt.Printf("Node %s approved (now online)\n", nodeID)
			return nil
		},
	}
}

func nodesRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove NODE_ID",
		Short: "Remove a node from the cluster",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := newStoreOnly()
			if err != nil {
				return err
			}

			nodeID := args[0]
			n := store.GetNode(nodeID)
			if n == nil {
				return fmt.Errorf("node %q not found", nodeID)
			}

			if err := store.RemoveNode(nodeID); err != nil {
				return fmt.Errorf("removing node: %w", err)
			}

			fmt.Printf("Node %s removed\n", nodeID)
			return nil
		},
	}
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
