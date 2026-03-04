// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build unit

package config

import (
	"math"
	"testing"
)

func FuzzParseMemory(f *testing.F) {
	// Valid inputs
	f.Add("512Mi")
	f.Add("1Gi")
	f.Add("256MB")
	f.Add("1024")
	f.Add("0.5Gi")
	f.Add("100KB")
	f.Add("1G")
	f.Add("1B")

	// Edge cases from previous audit passes
	f.Add("")
	f.Add("0")
	f.Add("0Mi")
	f.Add("09Mi")        // leading zeros
	f.Add("1.2.3Mi")     // multiple decimals
	f.Add("-1Gi")        // negative
	f.Add("NaN")         // not a number
	f.Add("Inf")         // infinity
	f.Add("InfGi")       // infinity with suffix
	f.Add("999999999Gi") // overflow candidate
	f.Add("0.0001B")     // truncates to zero
	f.Add("1Ti")         // invalid suffix for memory
	f.Add("abc")         // no numeric value
	f.Add("1X")          // unknown suffix
	f.Add("  512Mi  ")   // whitespace

	f.Fuzz(func(t *testing.T, input string) {
		result, err := ParseMemory(input)
		if err != nil {
			return // errors are expected for many inputs
		}

		// Invariant: successful parse must return positive value
		if result <= 0 {
			t.Errorf("ParseMemory(%q) = %d, want > 0", input, result)
		}

		// Invariant: result must be within float64 safe integer range
		if float64(result) > math.Exp2(53) {
			t.Errorf("ParseMemory(%q) = %d, exceeds 2^53", input, result)
		}
	})
}

func FuzzParseDiskSize(f *testing.F) {
	// Valid inputs
	f.Add("1G")
	f.Add("512M")
	f.Add("100Gi")
	f.Add("1Ti")
	f.Add("500GB")
	f.Add("0.5Ti")
	f.Add("1024")
	f.Add("1TB")

	// Edge cases from previous audit passes
	f.Add("")
	f.Add("0")
	f.Add("0Gi")
	f.Add("09Gi")        // leading zeros
	f.Add("1.2.3Gi")     // multiple decimals
	f.Add("-1Ti")        // negative
	f.Add("NaN")         // not a number
	f.Add("Inf")         // infinity
	f.Add("InfTi")       // infinity with suffix
	f.Add("999999999Ti") // overflow candidate
	f.Add("0.0001B")     // truncates to zero
	f.Add("abc")         // no numeric value
	f.Add("1X")          // unknown suffix
	f.Add("  1Ti  ")     // whitespace

	f.Fuzz(func(t *testing.T, input string) {
		result, err := ParseDiskSize(input)
		if err != nil {
			return // errors are expected for many inputs
		}

		// Invariant: successful parse must return positive value
		if result <= 0 {
			t.Errorf("ParseDiskSize(%q) = %d, want > 0", input, result)
		}

		// Invariant: result must be within float64 safe integer range
		if float64(result) > math.Exp2(53) {
			t.Errorf("ParseDiskSize(%q) = %d, exceeds 2^53", input, result)
		}
	})
}
