// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/brmurrell3/hive/internal/config"
	"github.com/brmurrell3/hive/internal/protocol"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
)

var (
	triggerTeam    string
	triggerPayload string
	triggerTimeout int
)

func triggerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trigger",
		Short: "Send a task to a team's lead agent",
		Long: `Publish a task message to a team's broadcast subject, triggering the
lead agent's orchestration logic. The payload is forwarded as the
capability inputs. Waits for the pipeline result and prints it.

Examples:
  hivectl trigger --team ci-pipeline --payload '{"repo_path": ".", "test_command": "go test ./..."}'
  hivectl trigger --cluster-root ./demo --team ci-pipeline --payload '{}'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTrigger()
		},
	}

	cmd.Flags().StringVar(&triggerTeam, "team", "", "Team to trigger (required)")
	cmd.Flags().StringVar(&triggerPayload, "payload", "{}", "JSON payload for the task")
	cmd.Flags().IntVar(&triggerTimeout, "timeout", 60, "Seconds to wait for pipeline result")
	cmd.MarkFlagRequired("team") //nolint:errcheck

	return cmd
}

func runTrigger() error {
	if triggerTeam == "" {
		return fmt.Errorf("--team flag is required")
	}

	// Validate the payload is valid JSON.
	var payloadMap map[string]interface{}
	if err := json.Unmarshal([]byte(triggerPayload), &payloadMap); err != nil {
		return fmt.Errorf("invalid JSON payload: %w", err)
	}

	absRoot, err := filepath.Abs(clusterRoot)
	if err != nil {
		return fmt.Errorf("resolving cluster root: %w", err)
	}

	// Load cluster config to get NATS connection info.
	cluster, err := config.LoadCluster(absRoot)
	if err != nil {
		return fmt.Errorf("loading cluster config: %w", err)
	}

	natsPort := cluster.Spec.NATS.Port
	if natsPort == 0 {
		natsPort = 4222
	}
	natsURL := fmt.Sprintf("nats://127.0.0.1:%d", natsPort)

	// Try to read NATS token from dev mode connection file.
	natsTokenStr := cluster.Spec.NATS.AuthToken
	if natsTokenStr == "" {
		connInfoPath := filepath.Join(absRoot, ".state", "nats.env")
		if data, err := os.ReadFile(connInfoPath); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, "HIVE_NATS_TOKEN=") {
					natsTokenStr = strings.TrimPrefix(line, "HIVE_NATS_TOKEN=")
				}
			}
		}
	}

	// Connect to NATS.
	opts := []nats.Option{nats.Timeout(5 * time.Second)}
	if natsTokenStr != "" {
		opts = append(opts, nats.Token(natsTokenStr))
	}

	nc, err := nats.Connect(natsURL, opts...)
	if err != nil {
		return fmt.Errorf("connecting to NATS at %s: %w", natsURL, err)
	}
	defer nc.Close()

	// Subscribe to the result subject before publishing the trigger,
	// so we don't miss the response.
	resultSubj := fmt.Sprintf(protocol.FmtTeamResult, triggerTeam)
	resultCh := make(chan []byte, 1)
	sub, err := nc.Subscribe(resultSubj, func(msg *nats.Msg) {
		select {
		case resultCh <- msg.Data:
		default:
		}
	})
	if err != nil {
		return fmt.Errorf("subscribing to result subject: %w", err)
	}
	defer sub.Unsubscribe() //nolint:errcheck

	// Build and publish the trigger envelope.
	payloadBytes, _ := json.Marshal(payloadMap)
	env := types.Envelope{
		ID:        types.NewUUID(),
		From:      "hivectl",
		To:        triggerTeam,
		Type:      types.MessageTypeTask,
		Timestamp: time.Now().UTC(),
		Payload:   payloadBytes,
	}

	envBytes, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshalling trigger envelope: %w", err)
	}

	subject := fmt.Sprintf(protocol.FmtTeamBroadcast, triggerTeam)
	if err := nc.Publish(subject, envBytes); err != nil {
		return fmt.Errorf("publishing trigger: %w", err)
	}
	if err := nc.Flush(); err != nil {
		return fmt.Errorf("flushing NATS: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Pipeline triggered. Waiting for result...\n")

	// Wait for the pipeline result.
	timeout := time.Duration(triggerTimeout) * time.Second
	select {
	case data := <-resultCh:
		// Pretty-print the JSON result.
		var result map[string]interface{}
		if err := json.Unmarshal(data, &result); err != nil {
			// Not valid JSON, print raw.
			fmt.Println(string(data))
			return nil
		}
		pretty, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(pretty))
	case <-time.After(timeout):
		return fmt.Errorf("timed out waiting for pipeline result after %s", timeout)
	}

	return nil
}
