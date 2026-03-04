package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func capabilitiesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "capabilities",
		Short: "Manage and invoke agent capabilities",
	}

	cmd.AddCommand(capabilitiesInvokeCmd())

	return cmd
}

func capabilitiesInvokeCmd() *cobra.Command {
	var inputs string

	cmd := &cobra.Command{
		Use:   "invoke AGENT_ID CAPABILITY_NAME",
		Short: "Invoke a capability on an agent",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Stub: actual invocation requires a live NATS connection.
			_ = args[0]
			_ = args[1]
			_ = inputs

			fmt.Println("capability invoke requires connection to running hived NATS server")
			return nil
		},
	}

	cmd.Flags().StringVar(&inputs, "inputs", "", "JSON string of capability inputs")

	return cmd
}
