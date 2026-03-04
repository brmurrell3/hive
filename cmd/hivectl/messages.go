package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func messagesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "messages",
		Short: "Send and subscribe to NATS messages",
	}

	cmd.AddCommand(messagesSendCmd())
	cmd.AddCommand(messagesSubscribeCmd())

	return cmd
}

func messagesSendCmd() *cobra.Command {
	var (
		from    string
		to      string
		payload string
	)

	cmd := &cobra.Command{
		Use:   "send",
		Short: "Send a message via NATS",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Stub: these flags are parsed but actual sending requires a live NATS connection.
			_ = from
			_ = to
			_ = payload

			fmt.Println("message send requires connection to running hived NATS server")
			return nil
		},
	}

	cmd.Flags().StringVar(&from, "from", "", "Sender agent ID (required)")
	cmd.Flags().StringVar(&to, "to", "", "Recipient agent ID (required)")
	cmd.Flags().StringVar(&payload, "payload", "", "Message payload (required)")
	_ = cmd.MarkFlagRequired("from")
	_ = cmd.MarkFlagRequired("to")
	_ = cmd.MarkFlagRequired("payload")

	return cmd
}

func messagesSubscribeCmd() *cobra.Command {
	var since string

	cmd := &cobra.Command{
		Use:   "subscribe SUBJECT",
		Short: "Subscribe to messages on a NATS subject",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Stub: actual subscribing requires a live NATS connection.
			_ = args[0]
			_ = since

			fmt.Println("message subscribe requires connection to running hived NATS server")
			return nil
		},
	}

	cmd.Flags().StringVar(&since, "since", "", "Only show messages from this duration ago (e.g., 5m, 1h)")

	return cmd
}
