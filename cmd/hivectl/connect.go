package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func connectCmd() *cobra.Command {
	var web bool

	cmd := &cobra.Command{
		Use:   "connect AGENT_ID",
		Short: "Connect to a running agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]

			if web {
				fmt.Printf("Web connect to agent %s requires running hived daemon\n", agentID)
			} else {
				fmt.Printf("Connect to agent %s requires running hived daemon\n", agentID)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&web, "web", false, "Open web-based connection to the agent")

	return cmd
}
