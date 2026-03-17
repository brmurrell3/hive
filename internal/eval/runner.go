// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

// Package eval implements the evaluation framework for measuring agent and team
// performance on repeatable tasks across different models and prompt strategies.
package eval

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/brmurrell3/hive/internal/types"
)

// TriggerFunc triggers a team with a payload and returns the structured result.
// This abstracts the NATS connection so the runner can be tested independently.
type TriggerFunc func(team string, payload map[string]interface{}, timeout time.Duration) (map[string]interface{}, error)

// ModelSwapFunc swaps the model for a team's agents before an eval run.
// It receives the provider and model name, and should update the running
// cluster configuration. Returns a cleanup function to restore the original.
type ModelSwapFunc func(team string, provider, model string) (cleanup func(), err error)

// Runner executes eval manifests against a running cluster.
type Runner struct {
	trigger   TriggerFunc
	modelSwap ModelSwapFunc
	logger    *slog.Logger
}

// NewRunner creates a new eval runner with the given trigger and model swap functions.
func NewRunner(trigger TriggerFunc, modelSwap ModelSwapFunc, logger *slog.Logger) *Runner {
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{
		trigger:   trigger,
		modelSwap: modelSwap,
		logger:    logger,
	}
}

// Run executes an eval manifest and returns the results.
func (r *Runner) Run(eval *types.EvalManifest) (*types.EvalResult, error) {
	result := &types.EvalResult{
		EvalName:  eval.Metadata.Name,
		Timestamp: time.Now().UTC(),
	}

	for _, model := range eval.Spec.Models {
		r.logger.Info("evaluating model",
			"provider", model.Provider,
			"model", model.Name,
			"cases", len(eval.Spec.Dataset),
			"runs_per_case", eval.Spec.Runs.Count,
		)

		// Swap the model if a swap function is provided.
		var cleanup func()
		if r.modelSwap != nil {
			var err error
			cleanup, err = r.modelSwap(eval.Metadata.Team, model.Provider, model.Name)
			if err != nil {
				return nil, fmt.Errorf("swapping model to %s/%s: %w", model.Provider, model.Name, err)
			}
		}

		modelResult, err := r.runModel(eval, model)
		if err != nil {
			if cleanup != nil {
				cleanup()
			}
			return nil, fmt.Errorf("running eval for %s/%s: %w", model.Provider, model.Name, err)
		}

		if cleanup != nil {
			cleanup()
		}

		result.Models = append(result.Models, *modelResult)
	}

	return result, nil
}

func (r *Runner) runModel(eval *types.EvalManifest, model types.EvalModel) (*types.EvalModelResult, error) {
	mr := &types.EvalModelResult{
		Provider: model.Provider,
		Model:    model.Name,
	}

	// Run test cases with bounded concurrency.
	sem := make(chan struct{}, eval.Spec.Runs.Parallel)
	var mu sync.Mutex
	var wg sync.WaitGroup

	type caseRun struct {
		tc  types.EvalTestCase
		run int
	}

	// Build the full list of (case, run) pairs.
	var work []caseRun
	for _, tc := range eval.Spec.Dataset {
		for run := 1; run <= eval.Spec.Runs.Count; run++ {
			work = append(work, caseRun{tc: tc, run: run})
		}
	}

	var firstErr error
	for _, w := range work {
		wg.Add(1)
		sem <- struct{}{}
		go func(cr caseRun) {
			defer wg.Done()
			defer func() { <-sem }()

			caseResult := r.runCase(eval, cr.tc, cr.run)

			mu.Lock()
			mr.Cases = append(mr.Cases, caseResult)
			if caseResult.Error != "" && firstErr == nil {
				firstErr = fmt.Errorf("case %q run %d: %s", cr.tc.Name, cr.run, caseResult.Error)
			}
			mu.Unlock()
		}(w)
	}
	wg.Wait()

	// Sort cases by name then run number for deterministic output.
	sort.Slice(mr.Cases, func(i, j int) bool {
		if mr.Cases[i].Name != mr.Cases[j].Name {
			return mr.Cases[i].Name < mr.Cases[j].Name
		}
		return mr.Cases[i].Run < mr.Cases[j].Run
	})

	// Compute summary metrics.
	mr.Summary = computeSummary(mr.Cases)

	return mr, nil
}

func (r *Runner) runCase(eval *types.EvalManifest, tc types.EvalTestCase, run int) types.EvalCaseResult {
	start := time.Now()

	result := types.EvalCaseResult{
		Name: tc.Name,
		Run:  run,
	}

	r.logger.Info("running test case",
		"case", tc.Name,
		"run", run,
	)

	output, err := r.trigger(eval.Metadata.Team, tc.Payload, eval.Spec.Runs.Timeout)
	result.LatencyMs = time.Since(start).Milliseconds()

	if err != nil {
		result.Error = err.Error()
		result.Passed = false
		return result
	}

	result.Output = output

	// Score assertions.
	allPassed := true
	for _, assertion := range tc.Expected.Assertions {
		ar := checkAssertion(output, assertion)
		result.Assertions = append(result.Assertions, ar)
		if !ar.Passed {
			allPassed = false
		}
	}

	// Check expected fields.
	for key, expected := range tc.Expected.Fields {
		actual, ok := output[key]
		ar := types.EvalAssertionResult{
			Type:     "equals",
			Path:     key,
			Expected: fmt.Sprintf("%v", expected),
			Actual:   fmt.Sprintf("%v", actual),
			Passed:   ok && fmt.Sprintf("%v", actual) == fmt.Sprintf("%v", expected),
		}
		result.Assertions = append(result.Assertions, ar)
		if !ar.Passed {
			allPassed = false
		}
	}

	result.Passed = allPassed
	return result
}

func checkAssertion(output map[string]interface{}, assertion types.EvalAssertion) types.EvalAssertionResult {
	ar := types.EvalAssertionResult{
		Type:     assertion.Type,
		Path:     assertion.Path,
		Expected: fmt.Sprintf("%v", assertion.Value),
	}

	// Resolve the value at the given path.
	actual := resolveJSONPath(output, assertion.Path)
	actualStr := fmt.Sprintf("%v", actual)
	ar.Actual = actualStr

	switch assertion.Type {
	case "equals":
		ar.Passed = actualStr == fmt.Sprintf("%v", assertion.Value)

	case "contains":
		expectedStr := fmt.Sprintf("%v", assertion.Value)
		ar.Passed = strings.Contains(actualStr, expectedStr)

	case "regex":
		pattern := fmt.Sprintf("%v", assertion.Value)
		re, err := regexp.Compile(pattern)
		if err != nil {
			ar.Passed = false
			ar.Actual = fmt.Sprintf("invalid regex: %v", err)
		} else {
			ar.Passed = re.MatchString(actualStr)
		}

	case "json_path":
		// json_path assertions check that the path exists and has a truthy value.
		ar.Passed = actual != nil && actualStr != "" && actualStr != "false" && actualStr != "0" && actualStr != "<nil>"

	default:
		ar.Passed = false
		ar.Actual = fmt.Sprintf("unsupported assertion type: %s", assertion.Type)
	}

	return ar
}

// resolveJSONPath resolves a dot-separated path into a nested map.
// E.g., "review.findings_count" resolves output["review"]["findings_count"].
func resolveJSONPath(data map[string]interface{}, path string) interface{} {
	parts := strings.Split(path, ".")
	var current interface{} = data

	for _, part := range parts {
		switch v := current.(type) {
		case map[string]interface{}:
			current = v[part]
		case json.RawMessage:
			var m map[string]interface{}
			if err := json.Unmarshal(v, &m); err != nil {
				return nil
			}
			current = m[part]
		default:
			return nil
		}
	}

	return current
}

func computeSummary(cases []types.EvalCaseResult) types.EvalMetricsSummary {
	summary := types.EvalMetricsSummary{
		TotalRuns: len(cases),
	}

	if len(cases) == 0 {
		return summary
	}

	latencies := make([]int64, 0, len(cases))
	for _, c := range cases {
		if c.Passed {
			summary.TotalPassed++
		}
		latencies = append(latencies, c.LatencyMs)
	}

	summary.Accuracy = float64(summary.TotalPassed) / float64(summary.TotalRuns)

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	summary.LatencyP50 = percentile(latencies, 50)
	summary.LatencyP99 = percentile(latencies, 99)

	return summary
}

func percentile(sorted []int64, p int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(float64(p)/100.0*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
