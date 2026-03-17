// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/brmurrell3/hive/internal/config"
	"github.com/brmurrell3/hive/internal/eval"
	"github.com/brmurrell3/hive/internal/protocol"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
)

var (
	evalRoot   string
	evalOutput string
)

func evalCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "eval",
		Short: "Run evaluations against agent teams",
		Long: `The eval framework measures agent and team performance on repeatable
tasks across different models. Define test cases with expected outputs
and scoring metrics in an eval manifest (eval.yaml).

Examples:
  # Run an eval
  hivectl eval run --eval-root evals/ci-pipeline-accuracy --cluster-root my-pipeline

  # Save results as JSON
  hivectl eval run --eval-root evals/ci-pipeline-accuracy --cluster-root my-pipeline --output json`,
	}

	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Execute an eval manifest against a running cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEval()
		},
	}

	runCmd.Flags().StringVar(&evalRoot, "eval-root", "", "Path to the eval directory containing eval.yaml (required)")
	runCmd.Flags().StringVar(&evalOutput, "output", "table", "Output format: table or json")
	runCmd.MarkFlagRequired("eval-root") //nolint:errcheck

	cmd.AddCommand(runCmd)
	return cmd
}

func runEval() error {
	absEvalRoot, err := filepath.Abs(evalRoot)
	if err != nil {
		return fmt.Errorf("resolving eval root: %w", err)
	}

	absClusterRoot, err := filepath.Abs(clusterRoot)
	if err != nil {
		return fmt.Errorf("resolving cluster root: %w", err)
	}

	// Load eval manifest.
	evalManifest, err := config.LoadEval(absEvalRoot)
	if err != nil {
		return fmt.Errorf("loading eval manifest: %w", err)
	}

	// Load cluster config for NATS connection.
	cluster, err := config.LoadCluster(absClusterRoot)
	if err != nil {
		return fmt.Errorf("loading cluster config: %w", err)
	}

	// Connect to NATS.
	nc, err := connectEvalNATS(cluster, absClusterRoot)
	if err != nil {
		return err
	}
	defer nc.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Create trigger function that publishes to the team and waits for results.
	triggerFn := func(team string, payload map[string]interface{}, timeout time.Duration) (map[string]interface{}, error) {
		return triggerTeamForEval(nc, team, payload, timeout)
	}

	runner := eval.NewRunner(triggerFn, nil, logger)

	fmt.Fprintf(os.Stderr, "Running eval %q against team %q (%d cases, %d models)\n",
		evalManifest.Metadata.Name,
		evalManifest.Metadata.Team,
		len(evalManifest.Spec.Dataset),
		len(evalManifest.Spec.Models),
	)

	result, err := runner.Run(evalManifest)
	if err != nil {
		return fmt.Errorf("running eval: %w", err)
	}

	// Output results.
	switch evalOutput {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(result); err != nil {
			return fmt.Errorf("encoding results: %w", err)
		}
	default:
		eval.FormatResultsTable(os.Stdout, result)
	}

	// Save results to .state/evals/ for historical comparison.
	if err := saveEvalResults(absClusterRoot, result); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to save eval results: %v\n", err)
	}

	return nil
}

func connectEvalNATS(cluster *types.ClusterConfig, clusterRoot string) (*nats.Conn, error) {
	natsPort := cluster.Spec.NATS.Port
	if natsPort == 0 {
		natsPort = 4222
	}
	natsURL := fmt.Sprintf("nats://127.0.0.1:%d", natsPort)

	natsTokenStr := cluster.Spec.NATS.AuthToken
	if natsTokenStr == "" {
		connInfoPath := filepath.Join(clusterRoot, ".state", "nats.env")
		if data, err := os.ReadFile(connInfoPath); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, "HIVE_NATS_TOKEN=") {
					natsTokenStr = strings.TrimPrefix(line, "HIVE_NATS_TOKEN=")
				}
			}
		}
	}

	opts := []nats.Option{nats.Timeout(5 * time.Second)}
	if natsTokenStr != "" {
		opts = append(opts, nats.Token(natsTokenStr))
	}

	nc, err := nats.Connect(natsURL, opts...)
	if err != nil {
		return nil, fmt.Errorf("connecting to NATS at %s: %w", natsURL, err)
	}
	return nc, nil
}

func triggerTeamForEval(nc *nats.Conn, team string, payload map[string]interface{}, timeout time.Duration) (map[string]interface{}, error) {
	// Subscribe to result before publishing.
	resultSubj := fmt.Sprintf(protocol.FmtTeamResult, team)
	resultCh := make(chan []byte, 1)
	sub, err := nc.Subscribe(resultSubj, func(msg *nats.Msg) {
		select {
		case resultCh <- msg.Data:
		default:
		}
	})
	if err != nil {
		return nil, fmt.Errorf("subscribing to result subject: %w", err)
	}
	defer sub.Unsubscribe() //nolint:errcheck

	// Build and publish trigger envelope.
	payloadBytes, _ := json.Marshal(payload)
	env := types.Envelope{
		ID:        types.NewUUID(),
		From:      "hivectl-eval",
		To:        team,
		Type:      types.MessageTypeTask,
		Timestamp: time.Now().UTC(),
		Payload:   payloadBytes,
	}

	envBytes, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("marshalling trigger envelope: %w", err)
	}

	subject := fmt.Sprintf(protocol.FmtTeamBroadcast, team)
	if err := nc.Publish(subject, envBytes); err != nil {
		return nil, fmt.Errorf("publishing trigger: %w", err)
	}
	if err := nc.Flush(); err != nil {
		return nil, fmt.Errorf("flushing NATS: %w", err)
	}

	// Wait for result.
	select {
	case data := <-resultCh:
		var result map[string]interface{}
		if err := json.Unmarshal(data, &result); err != nil {
			return nil, fmt.Errorf("parsing result: %w", err)
		}
		return result, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("timed out waiting for result after %s", timeout)
	}
}

func saveEvalResults(clusterRoot string, result *types.EvalResult) error {
	evalDir := filepath.Join(clusterRoot, ".state", "evals")
	if err := os.MkdirAll(evalDir, 0o755); err != nil {
		return fmt.Errorf("creating evals directory: %w", err)
	}

	filename := fmt.Sprintf("%s_%s.json",
		result.EvalName,
		result.Timestamp.Format("20060102-150405"),
	)
	path := filepath.Join(evalDir, filename)

	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling results: %w", err)
	}

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing results: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Results saved to %s\n", path)
	return nil
}
