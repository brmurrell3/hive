// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/brmurrell3/hive/internal/types"
	"gopkg.in/yaml.v3"
)

// LoadEval reads and parses an eval manifest from the given eval root directory.
// It expects eval.yaml (or eval.yml) at the root of the eval directory.
func LoadEval(evalRoot string) (*types.EvalManifest, error) {
	path := filepath.Join(evalRoot, "eval.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Try .yml extension as fallback.
			path = filepath.Join(evalRoot, "eval.yml")
			data, err = os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("reading eval manifest: no eval.yaml or eval.yml found in %s", evalRoot)
			}
		} else {
			return nil, fmt.Errorf("reading eval manifest: %w", err)
		}
	}
	return ParseEval(data)
}

// ParseEval parses eval manifest YAML content into an EvalManifest.
func ParseEval(data []byte) (*types.EvalManifest, error) {
	var raw rawEvalManifest
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing eval manifest: %w", err)
	}

	if raw.Kind != "Eval" {
		return nil, fmt.Errorf("expected kind Eval, got %q", raw.Kind)
	}
	if raw.APIVersion != "hive/v1" {
		return nil, fmt.Errorf("unsupported apiVersion %q", raw.APIVersion)
	}

	timeout, err := parseDurationOrDefault(raw.Spec.Runs.Timeout, 60*time.Second)
	if err != nil {
		return nil, fmt.Errorf("parsing spec.runs.timeout: %w", err)
	}

	eval := &types.EvalManifest{
		APIVersion: raw.APIVersion,
		Kind:       raw.Kind,
		Metadata:   raw.Metadata,
		Spec: types.EvalSpec{
			Scoring: raw.Spec.Scoring,
			Models:  raw.Spec.Models,
			Runs: types.EvalRuns{
				Count:    raw.Spec.Runs.Count,
				Parallel: raw.Spec.Runs.Parallel,
				Timeout:  timeout,
			},
		},
	}

	// Convert raw dataset entries.
	for _, rc := range raw.Spec.Dataset {
		tc := types.EvalTestCase{
			Name:    rc.Name,
			Payload: rc.Payload,
			Expected: types.EvalExpected{
				Fields:     rc.Expected.Fields,
				Assertions: rc.Expected.Assertions,
			},
		}
		eval.Spec.Dataset = append(eval.Spec.Dataset, tc)
	}

	// Apply defaults.
	if eval.Spec.Runs.Count == 0 {
		eval.Spec.Runs.Count = 1
	}
	if eval.Spec.Runs.Parallel == 0 {
		eval.Spec.Runs.Parallel = 1
	}

	if err := validateEval(eval); err != nil {
		return nil, err
	}

	return eval, nil
}

func validateEval(eval *types.EvalManifest) error {
	if eval.Metadata.Name == "" {
		return fmt.Errorf("eval metadata.name is required")
	}
	if eval.Metadata.Team == "" {
		return fmt.Errorf("eval metadata.team is required")
	}
	if len(eval.Spec.Dataset) == 0 {
		return fmt.Errorf("eval spec.dataset must contain at least one test case")
	}
	for i, tc := range eval.Spec.Dataset {
		if tc.Name == "" {
			return fmt.Errorf("eval spec.dataset[%d].name is required", i)
		}
	}
	if len(eval.Spec.Models) == 0 {
		return fmt.Errorf("eval spec.models must contain at least one model")
	}
	for i, m := range eval.Spec.Models {
		if m.Provider == "" {
			return fmt.Errorf("eval spec.models[%d].provider is required", i)
		}
		if m.Name == "" {
			return fmt.Errorf("eval spec.models[%d].name is required", i)
		}
	}
	for i, a := range eval.Spec.Dataset {
		for j, assertion := range a.Expected.Assertions {
			switch assertion.Type {
			case "contains", "equals", "regex", "json_path":
				// valid
			default:
				return fmt.Errorf("eval spec.dataset[%d].expected.assertions[%d].type: unsupported type %q", i, j, assertion.Type)
			}
		}
	}
	return nil
}

// rawEvalManifest mirrors EvalManifest but uses strings for durations.
type rawEvalManifest struct {
	APIVersion string             `yaml:"apiVersion"`
	Kind       string             `yaml:"kind"`
	Metadata   types.EvalMetadata `yaml:"metadata"`
	Spec       rawEvalSpec        `yaml:"spec"`
}

type rawEvalSpec struct {
	Dataset []rawEvalTestCase `yaml:"dataset"`
	Scoring types.EvalScoring `yaml:"scoring"`
	Runs    rawEvalRuns       `yaml:"runs"`
	Models  []types.EvalModel `yaml:"models"`
}

type rawEvalTestCase struct {
	Name     string                 `yaml:"name"`
	Payload  map[string]interface{} `yaml:"payload"`
	Expected rawEvalExpected        `yaml:"expected"`
}

type rawEvalExpected struct {
	Fields     map[string]interface{} `yaml:"fields,omitempty"`
	Assertions []types.EvalAssertion  `yaml:"assertions,omitempty"`
}

type rawEvalRuns struct {
	Count    int    `yaml:"count"`
	Parallel int    `yaml:"parallel"`
	Timeout  string `yaml:"timeout"`
}
