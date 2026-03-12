// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/brmurrell3/hive/internal/protocol"
	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
)

// execCmd creates the top-level "exec" command that runs a command inside
// an agent's runtime environment via the sidecar control handler.
func execCmd() *cobra.Command {
	var timeout int

	cmd := &cobra.Command{
		Use:   "exec AGENT_ID -- COMMAND [ARGS...]",
		Short: "Execute a command inside an agent's runtime",
		Long: `Execute a command inside an agent's sidecar runtime via NATS.

The command is sent to the agent's sidecar control handler on the NATS
subject hive.agent.{AGENT_ID}.sidecar.exec, which executes it in the
agent's runtime environment. Stdout and stderr are returned.

Examples:
  # Run a simple command
  hivectl exec my-agent -- ls -la /workspace

  # Execute a Python command
  hivectl exec my-agent -- python3 -c "print('hello')"

  # Run with a longer timeout
  hivectl exec my-agent --timeout 60 -- long-running-script.sh`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]

			if err := types.ValidateSubjectComponent("agent_id", agentID); err != nil {
				return err
			}

			// Extract the command after "--".
			var execArgs []string
			dashDash := cmd.ArgsLenAtDash()
			if dashDash >= 0 && dashDash < len(args) {
				execArgs = args[dashDash:]
			} else if len(args) > 1 {
				execArgs = args[1:]
			}

			if len(execArgs) == 0 {
				return fmt.Errorf("no command specified; use -- to separate the command from exec flags")
			}

			return runExec(agentID, execArgs, timeout)
		},
	}

	cmd.Flags().IntVar(&timeout, "timeout", 30, "Exec timeout in seconds")

	return cmd
}

// runExec sends an exec command to an agent's sidecar and prints the result.
func runExec(agentID string, execArgs []string, timeout int) error {
	// Verify agent is running.
	var agentState *state.AgentState
	if err := withClient(func(client *DaemonClient) error {
		var statusErr error
		agentState, statusErr = client.AgentStatus(agentID)
		if statusErr != nil {
			return fmt.Errorf("checking agent status: %w", statusErr)
		}
		return nil
	}); err != nil {
		return err
	}

	if agentState.Status != state.AgentStatusRunning {
		return fmt.Errorf("agent %q is not running (status: %s)", agentID, agentState.Status)
	}

	// Connect to NATS for the exec request.
	nc, err := connectNATS(fmt.Sprintf("hivectl-exec-%s", agentID))
	if err != nil {
		return err
	}
	defer func() { _ = nc.Drain() }()

	// Build exec request payload.
	payloadData, err := json.Marshal(map[string]interface{}{
		"action":  "exec",
		"command": execArgs,
	})
	if err != nil {
		return fmt.Errorf("marshaling exec payload: %w", err)
	}

	execReq := types.Envelope{
		ID:        types.NewUUID(),
		From:      "hivectl",
		To:        agentID,
		Type:      types.MessageTypeControl,
		Timestamp: time.Now().UTC(),
		Payload:   payloadData,
		UserToken: authToken,
	}

	envData, err := json.Marshal(execReq)
	if err != nil {
		return fmt.Errorf("marshaling exec request: %w", err)
	}

	// Send the exec request to the sidecar's control handler.
	subject := fmt.Sprintf(protocol.FmtAgentSidecarExec, agentID)
	resp, err := nc.Request(subject, envData, time.Duration(timeout)*time.Second)
	if err != nil {
		if errors.Is(err, nats.ErrTimeout) {
			return fmt.Errorf("exec timed out after %ds: agent sidecar may not support exec or is unresponsive", timeout)
		}
		return fmt.Errorf("exec request failed: %w", err)
	}

	// Parse the response.
	var execResp struct {
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		Error    string `json:"error,omitempty"`
	}
	if err := json.Unmarshal(resp.Data, &execResp); err != nil {
		// If we can't parse as structured response, print raw output.
		fmt.Print(string(resp.Data))
		return nil
	}

	if execResp.Error != "" {
		return fmt.Errorf("exec error: %s", execResp.Error)
	}

	if execResp.Stdout != "" {
		fmt.Print(execResp.Stdout)
	}
	if execResp.Stderr != "" {
		fmt.Fprint(os.Stderr, execResp.Stderr)
	}

	if execResp.ExitCode != 0 {
		return fmt.Errorf("command exited with code %d", execResp.ExitCode)
	}

	return nil
}
