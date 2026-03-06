package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/hivehq/hive/internal/state"
	"github.com/hivehq/hive/internal/types"
	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
)

func connectCmd() *cobra.Command {
	var web bool
	var timeout int

	cmd := &cobra.Command{
		Use:   "connect AGENT_ID",
		Short: "Connect to a running agent",
		Long: `Open an interactive message session with a running agent.

Messages are sent to the agent's NATS inbox and responses are printed
as they arrive. Type a message and press Enter to send. Press Ctrl+C to exit.

With --web, prints the URL for the agent's web dashboard interface instead.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]

			if web {
				return connectWeb(agentID)
			}
			return connectInteractive(agentID, timeout)
		},
	}

	cmd.Flags().BoolVar(&web, "web", false, "Print the web dashboard URL for this agent")
	cmd.Flags().IntVar(&timeout, "timeout", 30, "Response timeout in seconds")

	return cmd
}

// connectWeb prints the dashboard URL for the given agent.
func connectWeb(agentID string) error {
	// Verify the agent exists and is running.
	client, err := newDaemonClient()
	if err != nil {
		return fmt.Errorf("connecting to hived: %w", err)
	}
	defer client.Close()

	agentState, err := client.AgentStatus(agentID)
	if err != nil {
		return fmt.Errorf("checking agent status: %w", err)
	}

	if agentState.Status != state.AgentStatusRunning {
		return fmt.Errorf("agent %q is not running (status: %s)", agentID, agentState.Status)
	}

	// The dashboard runs on port 8080 by default.
	url := fmt.Sprintf("http://127.0.0.1:8080/agents/%s", agentID)
	fmt.Printf("Agent %q is running.\n", agentID)
	fmt.Printf("Dashboard URL: %s\n", url)
	fmt.Printf("\nOpen this URL in your browser to interact with the agent.\n")

	return nil
}

// connectInteractive opens an interactive NATS-based message session with the agent.
func connectInteractive(agentID string, timeoutSec int) error {
	// Step 1: Verify the agent is running via the daemon client.
	client, err := newDaemonClient()
	if err != nil {
		return fmt.Errorf("connecting to hived: %w", err)
	}

	agentState, err := client.AgentStatus(agentID)
	if err != nil {
		return fmt.Errorf("checking agent status: %w", err)
	}
	// Close the daemon client -- we will use a direct NATS connection for messaging.
	client.Close()

	if agentState.Status != state.AgentStatusRunning {
		return fmt.Errorf("agent %q is not running (status: %s)", agentID, agentState.Status)
	}

	// Step 2: Open a dedicated NATS connection for the interactive session.
	nc, err := connectNATS(fmt.Sprintf("hivectl-connect-%s", agentID))
	if err != nil {
		return err
	}
	defer func() {
		_ = nc.Drain()
	}()

	// Step 3: Subscribe to the agent's response subject for async replies.
	responseSubject := fmt.Sprintf("hivectl.connect.%s.%d", agentID, time.Now().UnixNano())
	responseCh := make(chan *nats.Msg, 16)

	sub, err := nc.Subscribe(responseSubject, func(msg *nats.Msg) {
		select {
		case responseCh <- msg:
		default:
			// Drop if channel is full (unlikely in interactive use).
		}
	})
	if err != nil {
		return fmt.Errorf("subscribing to response subject: %w", err)
	}
	defer func() {
		_ = sub.Unsubscribe()
	}()

	inboxSubject := fmt.Sprintf("hive.agent.%s.inbox", agentID)
	responseDuration := time.Duration(timeoutSec) * time.Second

	fmt.Printf("Connected to agent %q (status: %s)\n", agentID, agentState.Status)
	fmt.Printf("Type a message and press Enter to send. Press Ctrl+C to exit.\n")
	fmt.Println()

	// Step 4: Handle Ctrl+C gracefully.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)

	// Step 5: Read from stdin in a loop, send each line as a task message.
	scanner := bufio.NewScanner(os.Stdin)

	for {
		fmt.Print("> ")

		// Use a goroutine-based approach to allow interrupt during blocking stdin read.
		lineCh := make(chan string, 1)
		errCh := make(chan error, 1)

		go func() {
			if scanner.Scan() {
				lineCh <- scanner.Text()
			} else {
				if err := scanner.Err(); err != nil {
					errCh <- err
				} else {
					errCh <- fmt.Errorf("EOF")
				}
			}
		}()

		select {
		case <-sigCh:
			fmt.Println("\nDisconnected.")
			return nil

		case err := <-errCh:
			if err.Error() == "EOF" {
				fmt.Println("\nDisconnected.")
				return nil
			}
			return fmt.Errorf("reading input: %w", err)

		case line := <-lineCh:
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}

			// Build and send the envelope.
			envelope := types.Envelope{
				ID:        types.NewUUID(),
				From:      "hivectl",
				To:        agentID,
				Type:      types.MessageTypeTask,
				Timestamp: time.Now().UTC(),
				Payload:   map[string]string{"message": line},
				ReplyTo:   responseSubject,
			}

			envData, err := json.Marshal(envelope)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error marshaling message: %v\n", err)
				continue
			}

			if err := nc.Publish(inboxSubject, envData); err != nil {
				fmt.Fprintf(os.Stderr, "Error sending message: %v\n", err)
				continue
			}

			// Wait for a response with timeout.
			select {
			case msg := <-responseCh:
				printResponse(msg.Data)

			case <-time.After(responseDuration):
				fmt.Fprintf(os.Stderr, "No response from agent within %ds\n", timeoutSec)

			case <-sigCh:
				fmt.Println("\nDisconnected.")
				return nil
			}
		}
	}
}

// printResponse parses and displays a response from the agent.
func printResponse(data []byte) {
	// Try to parse as an envelope first.
	var env types.Envelope
	if err := json.Unmarshal(data, &env); err == nil && env.Payload != nil {
		// Try to extract a human-readable response from the payload.
		switch p := env.Payload.(type) {
		case map[string]interface{}:
			if msg, ok := p["message"]; ok {
				fmt.Printf("< %v\n", msg)
				return
			}
			if result, ok := p["result"]; ok {
				fmt.Printf("< %v\n", result)
				return
			}
			if answer, ok := p["answer"]; ok {
				fmt.Printf("< %v\n", answer)
				return
			}
			// Fall through to print the full payload as JSON.
			payloadJSON, err := json.MarshalIndent(p, "  ", "  ")
			if err == nil {
				fmt.Printf("< %s\n", payloadJSON)
				return
			}
		case string:
			fmt.Printf("< %s\n", p)
			return
		}

		// Generic payload rendering.
		payloadJSON, err := json.Marshal(env.Payload)
		if err == nil {
			fmt.Printf("< %s\n", payloadJSON)
			return
		}
	}

	// If we cannot parse as envelope, print raw.
	fmt.Printf("< %s\n", string(data))
}
