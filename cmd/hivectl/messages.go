package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/hivehq/hive/internal/types"
	"github.com/nats-io/nats.go"
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
			// Connect to hived's NATS server.
			nc, err := connectNATS("hivectl-messages-send")
			if err != nil {
				return err
			}
			defer func() {
				_ = nc.Drain()
			}()

			// Construct the envelope.
			env := types.Envelope{
				ID:        types.NewUUID(),
				From:      from,
				To:        to,
				Type:      types.MessageTypeTask,
				Timestamp: time.Now().UTC(),
				Payload:   map[string]string{"message": payload},
			}

			data, err := json.Marshal(env)
			if err != nil {
				return fmt.Errorf("marshaling envelope: %w", err)
			}

			// Publish to the target agent's inbox subject.
			subject := fmt.Sprintf("hive.agent.%s.inbox", to)
			if err := nc.Publish(subject, data); err != nil {
				return fmt.Errorf("publishing message to %s: %w", subject, err)
			}

			if err := nc.Flush(); err != nil {
				return fmt.Errorf("flushing NATS connection: %w", err)
			}

			fmt.Printf("Message sent to %s on subject %s (id: %s)\n", to, subject, env.ID)
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
			subject := args[0]

			// Connect to hived's NATS server.
			nc, err := connectNATS("hivectl-messages-subscribe")
			if err != nil {
				return err
			}
			defer func() {
				_ = nc.Drain()
			}()

			// Parse --since if provided, to filter messages by timestamp.
			var sinceTime time.Time
			if since != "" {
				dur, err := time.ParseDuration(since)
				if err != nil {
					return fmt.Errorf("invalid --since duration %q: %w", since, err)
				}
				sinceTime = time.Now().Add(-dur)
			}

			// Subscribe to the specified subject.
			sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
				// If --since is set, try to filter by envelope timestamp.
				if !sinceTime.IsZero() {
					var env types.Envelope
					if err := json.Unmarshal(msg.Data, &env); err == nil {
						if !env.Timestamp.IsZero() && env.Timestamp.Before(sinceTime) {
							return
						}
					}
				}

				// Pretty-print the message as indented JSON if it's valid JSON,
				// otherwise print the raw data.
				var raw json.RawMessage
				if err := json.Unmarshal(msg.Data, &raw); err == nil {
					formatted, err := json.MarshalIndent(raw, "", "  ")
					if err == nil {
						fmt.Println(string(formatted))
						return
					}
				}
				fmt.Println(string(msg.Data))
			})
			if err != nil {
				return fmt.Errorf("subscribing to %s: %w", subject, err)
			}
			defer func() {
				_ = sub.Unsubscribe()
			}()

			fmt.Fprintf(os.Stderr, "Subscribed to %s (press Ctrl+C to stop)...\n", subject)

			// Block until Ctrl+C.
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, os.Interrupt)
			<-sigCh

			fmt.Fprintf(os.Stderr, "\nUnsubscribed from %s\n", subject)
			return nil
		},
	}

	cmd.Flags().StringVar(&since, "since", "", "Only show messages from this duration ago (e.g., 5m, 1h)")

	return cmd
}
