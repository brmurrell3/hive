// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package eval

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/brmurrell3/hive/internal/types"
)

// FormatResultsTable writes a human-readable results table to the given writer.
// Format: Model | Accuracy | FP Rate | p50 Latency | p99 Latency | Runs
func FormatResultsTable(w io.Writer, result *types.EvalResult) {
	fmt.Fprintf(w, "Eval: %s\n", result.EvalName)
	fmt.Fprintf(w, "Time: %s\n\n", result.Timestamp.Format("2006-01-02 15:04:05 UTC"))

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "Model\tAccuracy\tp50 Latency\tp99 Latency\tPassed\tTotal")
	fmt.Fprintln(tw, "-----\t--------\t-----------\t-----------\t------\t-----")

	for _, mr := range result.Models {
		modelName := mr.Model
		if mr.Provider != "" {
			modelName = mr.Provider + "/" + mr.Model
		}
		fmt.Fprintf(tw, "%s\t%.0f%%\t%s\t%s\t%d\t%d\n",
			modelName,
			mr.Summary.Accuracy*100,
			formatLatency(mr.Summary.LatencyP50),
			formatLatency(mr.Summary.LatencyP99),
			mr.Summary.TotalPassed,
			mr.Summary.TotalRuns,
		)
	}
	tw.Flush()

	// Print per-case detail.
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Per-case results:")
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "Model\tCase\tRun\tPassed\tLatency\tAssertions")
	fmt.Fprintln(tw, "-----\t----\t---\t------\t-------\t----------")

	for _, mr := range result.Models {
		modelName := mr.Model
		if mr.Provider != "" {
			modelName = mr.Provider + "/" + mr.Model
		}
		for _, cr := range mr.Cases {
			passStr := "PASS"
			if !cr.Passed {
				passStr = "FAIL"
			}
			assertSummary := formatAssertions(cr.Assertions)
			fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\n",
				modelName,
				cr.Name,
				cr.Run,
				passStr,
				formatLatency(cr.LatencyMs),
				assertSummary,
			)
		}
	}
	tw.Flush()
}

func formatLatency(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return fmt.Sprintf("%.1fs", float64(ms)/1000.0)
}

func formatAssertions(assertions []types.EvalAssertionResult) string {
	if len(assertions) == 0 {
		return "-"
	}
	passed := 0
	for _, a := range assertions {
		if a.Passed {
			passed++
		}
	}
	if passed == len(assertions) {
		return fmt.Sprintf("%d/%d passed", passed, len(assertions))
	}

	// Show which failed.
	var failed []string
	for _, a := range assertions {
		if !a.Passed {
			failed = append(failed, fmt.Sprintf("%s(%s)", a.Path, a.Type))
		}
	}
	return fmt.Sprintf("%d/%d passed [failed: %s]", passed, len(assertions), strings.Join(failed, ", "))
}
