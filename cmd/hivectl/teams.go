package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/hivehq/hive/internal/config"
	"github.com/hivehq/hive/internal/types"
	"github.com/spf13/cobra"
)

func teamsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "teams",
		Short: "Manage teams",
	}

	cmd.AddCommand(teamsListCmd())
	cmd.AddCommand(teamsStatusCmd())
	cmd.AddCommand(teamsCapabilitiesCmd())

	return cmd
}

func teamsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all teams",
		RunE: func(cmd *cobra.Command, args []string) error {
			absRoot, err := filepath.Abs(clusterRoot)
			if err != nil {
				return fmt.Errorf("resolving cluster root: %w", err)
			}

			teams, err := config.LoadTeams(absRoot)
			if err != nil {
				return fmt.Errorf("loading teams: %w", err)
			}

			agents, err := config.LoadAgents(absRoot)
			if err != nil {
				return fmt.Errorf("loading agents: %w", err)
			}

			// Count members per team.
			memberCount := make(map[string]int)
			for _, a := range agents {
				if a.Metadata.Team != "" {
					memberCount[a.Metadata.Team]++
				}
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "TEAM_ID\tLEAD\tMEMBERS")

			for id, t := range teams {
				lead := t.Spec.Lead
				if lead == "" {
					lead = "-"
				}
				fmt.Fprintf(w, "%s\t%s\t%d\n", id, lead, memberCount[id])
			}

			w.Flush()
			return nil
		},
	}
}

func teamsStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status TEAM_ID",
		Short: "Show detailed status for a team",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			teamID := args[0]

			absRoot, err := filepath.Abs(clusterRoot)
			if err != nil {
				return fmt.Errorf("resolving cluster root: %w", err)
			}

			teams, err := config.LoadTeams(absRoot)
			if err != nil {
				return fmt.Errorf("loading teams: %w", err)
			}

			team, ok := teams[teamID]
			if !ok {
				fmt.Fprintf(os.Stderr, "Error: team %q not found\n", teamID)
				os.Exit(1)
			}

			data, err := json.MarshalIndent(team, "", "  ")
			if err != nil {
				return fmt.Errorf("marshaling team manifest: %w", err)
			}
			fmt.Println(string(data))
			return nil
		},
	}
}

func teamsCapabilitiesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "capabilities TEAM_ID",
		Short: "List all capabilities for agents in a team",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			teamID := args[0]

			absRoot, err := filepath.Abs(clusterRoot)
			if err != nil {
				return fmt.Errorf("resolving cluster root: %w", err)
			}

			teams, err := config.LoadTeams(absRoot)
			if err != nil {
				return fmt.Errorf("loading teams: %w", err)
			}

			if _, ok := teams[teamID]; !ok {
				fmt.Fprintf(os.Stderr, "Error: team %q not found\n", teamID)
				os.Exit(1)
			}

			agents, err := config.LoadAgents(absRoot)
			if err != nil {
				return fmt.Errorf("loading agents: %w", err)
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "AGENT\tNAME\tDESCRIPTION\tASYNC\tINPUTS\tOUTPUTS")

			for _, a := range agents {
				if a.Metadata.Team != teamID {
					continue
				}
				for _, cap := range a.Spec.Capabilities {
					inputs := formatParamNames(cap.Inputs)
					outputs := formatParamNames(cap.Outputs)
					async := "false"
					if cap.Async {
						async = "true"
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
						a.Metadata.ID,
						cap.Name,
						cap.Description,
						async,
						inputs,
						outputs,
					)
				}
			}

			w.Flush()
			return nil
		},
	}
}

// formatParamNames returns a comma-separated list of parameter names, or "-" if empty.
func formatParamNames(params []types.CapabilityParam) string {
	if len(params) == 0 {
		return "-"
	}
	names := make([]string, len(params))
	for i, p := range params {
		names[i] = p.Name
	}
	return strings.Join(names, ", ")
}
