package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/hivehq/hive/internal/capability"
	"github.com/hivehq/hive/internal/types"
	"github.com/nats-io/nats.go"
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
	var (
		inputs  string
		timeout int
	)

	cmd := &cobra.Command{
		Use:   "invoke AGENT_ID CAPABILITY_NAME",
		Short: "Invoke a capability on an agent",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]
			capName := args[1]

			// Parse inputs JSON if provided.
			var inputMap map[string]interface{}
			if inputs != "" {
				if err := json.Unmarshal([]byte(inputs), &inputMap); err != nil {
					return fmt.Errorf("parsing --inputs JSON: %w", err)
				}
			}

			// Connect to hived's NATS server.
			nc, err := connectNATS("hivectl-capabilities-invoke")
			if err != nil {
				return err
			}
			defer func() {
				_ = nc.Drain()
			}()

			// Construct the capability invocation request envelope.
			timeoutDur := time.Duration(timeout) * time.Second

			req := capability.InvocationRequest{
				Capability: capName,
				Inputs:     inputMap,
				Timeout:    timeoutDur.String(),
			}

			env := types.Envelope{
				ID:        types.NewUUID(),
				From:      "hivectl",
				To:        agentID,
				Type:      types.MessageTypeCapabilityRequest,
				Timestamp: time.Now().UTC(),
				Payload:   req,
			}

			data, err := json.Marshal(env)
			if err != nil {
				return fmt.Errorf("marshaling capability request: %w", err)
			}

			// Publish via NATS Request to wait for a reply.
			subject := fmt.Sprintf("hive.capabilities.%s.%s.request", agentID, capName)

			msg, err := nc.Request(subject, data, timeoutDur)
			if err != nil {
				if err == nats.ErrTimeout {
					return fmt.Errorf("capability invocation timed out after %ds: no response from agent %q for capability %q", timeout, agentID, capName)
				}
				return fmt.Errorf("invoking capability %s on %s: %w", capName, agentID, err)
			}

			// Parse the response envelope and extract the invocation response.
			var respEnv types.Envelope
			if err := json.Unmarshal(msg.Data, &respEnv); err != nil {
				return fmt.Errorf("parsing response envelope: %w", err)
			}

			// Re-marshal/unmarshal payload to get typed InvocationResponse.
			payloadBytes, err := json.Marshal(respEnv.Payload)
			if err != nil {
				return fmt.Errorf("processing response payload: %w", err)
			}

			var resp capability.InvocationResponse
			if err := json.Unmarshal(payloadBytes, &resp); err != nil {
				return fmt.Errorf("parsing invocation response: %w", err)
			}

			// Print the response as formatted JSON.
			output, err := json.MarshalIndent(resp, "", "  ")
			if err != nil {
				return fmt.Errorf("formatting response: %w", err)
			}
			fmt.Println(string(output))

			// Return an error if the invocation itself reported failure.
			if resp.Status == "error" || resp.Status == "timeout" {
				if resp.Error != nil {
					return fmt.Errorf("capability %s returned %s: %s", capName, resp.Status, resp.Error.Message)
				}
				return fmt.Errorf("capability %s returned %s", capName, resp.Status)
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&inputs, "inputs", "", "JSON string of capability inputs")
	cmd.Flags().IntVar(&timeout, "timeout", 30, "Response timeout in seconds")

	return cmd
}
