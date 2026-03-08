// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/brmurrell3/hive/internal/capability"
	"github.com/brmurrell3/hive/internal/protocol"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
)

func capabilitiesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "capabilities",
		Short: "Manage and invoke agent capabilities",
	}

	cmd.AddCommand(capabilitiesInvokeCmd())
	cmd.AddCommand(capabilitiesDescribeCmd())
	cmd.AddCommand(capabilitiesProvidersCmd())
	cmd.AddCommand(capabilitiesListCmd())

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
		Long: `Send a capability invocation request to a running agent over NATS and print
the JSON response. Input parameters are supplied as a JSON object via
--inputs; omit the flag when the capability takes no inputs.

Examples:
  # Invoke a capability with no inputs
  hivectl capabilities invoke analyst summarize
  # Output: {"status":"success","outputs":{"summary":"..."}}

  # Invoke a capability with inputs and a custom timeout
  hivectl capabilities invoke analyst run_query \
      --inputs '{"query":"SELECT * FROM trades LIMIT 10"}' --timeout 60
  # Output: {"status":"success","outputs":{"rows":[...]}}`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]
			capName := args[1]

			if err := types.ValidateSubjectComponent("agent_id", agentID); err != nil {
				return err
			}
			if err := types.ValidateSubjectComponent("capability_name", capName); err != nil {
				return err
			}

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
			if timeoutDur <= 0 {
				return fmt.Errorf("timeout must be a positive number of seconds (got %d)", timeout)
			}

			req := capability.InvocationRequest{
				Capability: capName,
				Inputs:     inputMap,
				Timeout:    timeoutDur.String(),
			}

			reqBytes, err := json.Marshal(req)
			if err != nil {
				return fmt.Errorf("marshaling capability request payload: %w", err)
			}

			env := types.Envelope{
				ID:        types.NewUUID(),
				From:      "hivectl",
				To:        agentID,
				Type:      types.MessageTypeCapabilityRequest,
				Timestamp: time.Now().UTC(),
				Payload:   reqBytes,
			}

			data, err := json.Marshal(env)
			if err != nil {
				return fmt.Errorf("marshaling capability request: %w", err)
			}

			// Publish via NATS Request to wait for a reply.
			subject := fmt.Sprintf(protocol.FmtCapabilityReq, agentID, capName)

			msg, err := nc.Request(subject, data, timeoutDur)
			if err != nil {
				if errors.Is(err, nats.ErrTimeout) {
					return fmt.Errorf("capability invocation timed out after %ds: no response from agent %q for capability %q", timeout, agentID, capName)
				}
				return fmt.Errorf("invoking capability %s on %s: %w", capName, agentID, err)
			}

			// Parse the response envelope and extract the invocation response.
			var respEnv types.Envelope
			if err := json.Unmarshal(msg.Data, &respEnv); err != nil {
				return fmt.Errorf("parsing response envelope: %w", err)
			}

			// Unmarshal payload to get typed InvocationResponse.
			var resp capability.InvocationResponse
			if err := json.Unmarshal(respEnv.Payload, &resp); err != nil {
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

func capabilitiesListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all registered capabilities across agents",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(func(client *DaemonClient) error {
				resp, err := client.request(protocol.SubjCapabilityList, nil)
				if err != nil {
					return fmt.Errorf("requesting capabilities list: %w", err)
				}
				if err := resp.Err(); err != nil {
					return err
				}

				var caps []struct {
					Name    string `json:"name"`
					AgentID string `json:"agent_id"`
					Team    string `json:"team,omitempty"`
				}
				if err := json.Unmarshal(resp.Data, &caps); err != nil {
					return fmt.Errorf("parsing response: %w", err)
				}

				if len(caps) == 0 {
					fmt.Println("No capabilities registered.")
					return nil
				}

				for _, c := range caps {
					team := ""
					if c.Team != "" {
						team = fmt.Sprintf(" [team: %s]", c.Team)
					}
					fmt.Printf("  %s  (agent: %s)%s\n", c.Name, c.AgentID, team)
				}
				return nil
			})
		},
	}
}

func capabilitiesDescribeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "describe CAPABILITY_NAME",
		Short: "Describe a capability including its inputs and outputs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			capName := args[0]

			if err := types.ValidateSubjectComponent("capability_name", capName); err != nil {
				return err
			}

			return withClient(func(client *DaemonClient) error {
				resp, err := client.request(protocol.SubjCapabilityDescribe, map[string]string{"name": capName})
				if err != nil {
					return fmt.Errorf("requesting capability description: %w", err)
				}
				if err := resp.Err(); err != nil {
					return err
				}

				var cap types.AgentCapability
				if err := json.Unmarshal(resp.Data, &cap); err != nil {
					return fmt.Errorf("parsing response: %w", err)
				}

				fmt.Printf("Name:        %s\n", cap.Name)
				fmt.Printf("Description: %s\n", cap.Description)

				if len(cap.Inputs) > 0 {
					fmt.Println("\nInputs:")
					for _, in := range cap.Inputs {
						required := ""
						if in.IsRequired() {
							required = " (required)"
						}
						fmt.Printf("  %s [%s]%s - %s\n", in.Name, in.Type, required, in.Description)
					}
				}
				if len(cap.Outputs) > 0 {
					fmt.Println("\nOutputs:")
					for _, out := range cap.Outputs {
						fmt.Printf("  %s [%s] - %s\n", out.Name, out.Type, out.Description)
					}
				}
				return nil
			})
		},
	}
}

func capabilitiesProvidersCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "providers CAPABILITY_NAME",
		Short: "List agents that provide a capability",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			capName := args[0]

			if err := types.ValidateSubjectComponent("capability_name", capName); err != nil {
				return err
			}

			return withClient(func(client *DaemonClient) error {
				resp, err := client.request(protocol.SubjCapabilityProviders, map[string]string{"name": capName})
				if err != nil {
					return fmt.Errorf("requesting capability providers: %w", err)
				}
				if err := resp.Err(); err != nil {
					return err
				}

				var providers []struct {
					AgentID string `json:"agent_id"`
					Team    string `json:"team,omitempty"`
					Status  string `json:"status"`
				}
				if err := json.Unmarshal(resp.Data, &providers); err != nil {
					return fmt.Errorf("parsing response: %w", err)
				}

				if len(providers) == 0 {
					fmt.Printf("No agents provide capability %q.\n", capName)
					return nil
				}

				fmt.Printf("Providers of %q:\n", capName)
				for _, p := range providers {
					team := ""
					if p.Team != "" {
						team = fmt.Sprintf(" [team: %s]", p.Team)
					}
					fmt.Printf("  %s (%s)%s\n", p.AgentID, p.Status, team)
				}
				return nil
			})
		},
	}
}
