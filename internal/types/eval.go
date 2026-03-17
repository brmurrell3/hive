// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package types

import "time"

// EvalManifest represents the parsed eval manifest (evals/EVAL_NAME/eval.yaml).
type EvalManifest struct {
	APIVersion string       `yaml:"apiVersion" json:"apiVersion"`
	Kind       string       `yaml:"kind" json:"kind"`
	Metadata   EvalMetadata `yaml:"metadata" json:"metadata"`
	Spec       EvalSpec     `yaml:"spec" json:"spec"`
}

// EvalMetadata holds identifying information for an eval.
type EvalMetadata struct {
	Name string `yaml:"name" json:"name"`
	Team string `yaml:"team" json:"team"`
}

// EvalSpec defines the eval configuration.
type EvalSpec struct {
	Dataset []EvalTestCase `yaml:"dataset" json:"dataset"`
	Scoring EvalScoring    `yaml:"scoring" json:"scoring"`
	Runs    EvalRuns       `yaml:"runs" json:"runs"`
	Models  []EvalModel    `yaml:"models" json:"models"`
}

// EvalTestCase is a single test case in the eval dataset.
type EvalTestCase struct {
	Name     string                 `yaml:"name" json:"name"`
	Payload  map[string]interface{} `yaml:"payload" json:"payload"`
	Expected EvalExpected           `yaml:"expected" json:"expected"`
}

// EvalExpected defines ground truth for scoring a test case.
type EvalExpected struct {
	Fields     map[string]interface{} `yaml:"fields,omitempty" json:"fields,omitempty"`
	Assertions []EvalAssertion        `yaml:"assertions,omitempty" json:"assertions,omitempty"`
}

// EvalAssertion is a single assertion rule for scoring.
type EvalAssertion struct {
	Type  string      `yaml:"type" json:"type"`   // contains, equals, regex, json_path
	Path  string      `yaml:"path" json:"path"`   // JSON path into agent output
	Value interface{} `yaml:"value" json:"value"` // expected value
}

// EvalScoring defines the scoring configuration.
type EvalScoring struct {
	Metrics []EvalMetric `yaml:"metrics" json:"metrics"`
}

// EvalMetric defines a single scoring metric.
type EvalMetric struct {
	Name string `yaml:"name" json:"name"` // e.g., "task_completion", "latency_p99"
	Type string `yaml:"type" json:"type"` // accuracy, latency, cost, custom
}

// EvalRuns defines how many times to run each case and concurrency settings.
type EvalRuns struct {
	Count    int           `yaml:"count" json:"count"`
	Parallel int           `yaml:"parallel" json:"parallel"`
	Timeout  time.Duration `yaml:"timeout" json:"timeout"`
}

// EvalModel defines a model to evaluate against.
type EvalModel struct {
	Provider string `yaml:"provider" json:"provider"`
	Name     string `yaml:"name" json:"name"`
}

// EvalResult holds the results from a single eval run.
type EvalResult struct {
	EvalName  string            `json:"eval_name"`
	Timestamp time.Time         `json:"timestamp"`
	Models    []EvalModelResult `json:"models"`
}

// EvalModelResult holds results for a single model across all test cases.
type EvalModelResult struct {
	Provider string             `json:"provider"`
	Model    string             `json:"model"`
	Cases    []EvalCaseResult   `json:"cases"`
	Summary  EvalMetricsSummary `json:"summary"`
}

// EvalCaseResult holds the result for a single test case run.
type EvalCaseResult struct {
	Name       string                 `json:"name"`
	Run        int                    `json:"run"`
	Passed     bool                   `json:"passed"`
	Output     map[string]interface{} `json:"output,omitempty"`
	Assertions []EvalAssertionResult  `json:"assertions"`
	LatencyMs  int64                  `json:"latency_ms"`
	Error      string                 `json:"error,omitempty"`
}

// EvalAssertionResult holds the result of a single assertion check.
type EvalAssertionResult struct {
	Type     string `json:"type"`
	Path     string `json:"path"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
	Passed   bool   `json:"passed"`
}

// EvalMetricsSummary holds aggregated metrics for a model's eval results.
type EvalMetricsSummary struct {
	Accuracy    float64 `json:"accuracy"`       // fraction of test cases passed
	LatencyP50  int64   `json:"latency_p50_ms"` // median latency
	LatencyP99  int64   `json:"latency_p99_ms"` // p99 latency
	TotalRuns   int     `json:"total_runs"`
	TotalPassed int     `json:"total_passed"`
}
