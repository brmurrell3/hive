package main

import (
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/brmurrell3/hive/internal/config"
	"github.com/brmurrell3/hive/internal/state"
	"github.com/spf13/cobra"
)

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show cluster overview",
		RunE: func(cmd *cobra.Command, args []string) error {
			absRoot, err := filepath.Abs(clusterRoot)
			if err != nil {
				return fmt.Errorf("resolving cluster root: %w", err)
			}

			cluster, err := config.LoadCluster(absRoot)
			if err != nil {
				return fmt.Errorf("loading cluster config: %w", err)
			}

			store, err := newStoreOnly()
			if err != nil {
				return fmt.Errorf("opening state store: %w", err)
			}

			agents := store.AllAgents()
			nodes := store.AllNodes()

			teams, err := config.LoadTeams(absRoot)
			if err != nil {
				return fmt.Errorf("loading teams: %w", err)
			}

			// Count agents by status.
			statusCounts := make(map[state.AgentStatus]int)
			for _, a := range agents {
				statusCounts[a.Status]++
			}

			natsStatus := "configured"
			if cluster.Spec.NATS.Port > 0 {
				natsStatus = fmt.Sprintf("configured (port %d)", cluster.Spec.NATS.Port)
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintf(w, "Cluster:\t%s\n", cluster.Metadata.Name)
			fmt.Fprintf(w, "Nodes:\t%d\n", len(nodes))
			fmt.Fprintf(w, "Teams:\t%d\n", len(teams))
			fmt.Fprintf(w, "Agents:\t%d\n", len(agents))

			if len(agents) > 0 {
				for _, s := range []state.AgentStatus{
					state.AgentStatusRunning,
					state.AgentStatusStopped,
					state.AgentStatusFailed,
					state.AgentStatusPending,
					state.AgentStatusCreating,
					state.AgentStatusStarting,
					state.AgentStatusStopping,
				} {
					if count, ok := statusCounts[s]; ok && count > 0 {
						fmt.Fprintf(w, "  %s:\t%d\n", s, count)
					}
				}
			}

			fmt.Fprintf(w, "NATS:\t%s\n", natsStatus)
			w.Flush()

			return nil
		},
	}
}
