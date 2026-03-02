// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package types

import (
	"fmt"
	"regexp"
	"strings"
)

// ValidateSubjectComponent validates that a string is safe for use as a single
// NATS subject token. Rejects characters that have special meaning in NATS:
// . > * space and control chars. Only alphanumeric, underscore, and hyphen are allowed.
var validSubjectComponent = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// MaxSubjectComponentLength is the maximum allowed length for a single NATS subject component.
const MaxSubjectComponentLength = 255

// MaxSubjectFieldLength is the maximum allowed length for a NATS subject field (may contain dots).
const MaxSubjectFieldLength = 512

func ValidateSubjectComponent(name, value string) error {
	if value == "" {
		return fmt.Errorf("%s must not be empty", name)
	}
	if len(value) > MaxSubjectComponentLength {
		return fmt.Errorf("%s exceeds maximum length of %d bytes: %q", name, MaxSubjectComponentLength, value)
	}
	if !validSubjectComponent.MatchString(value) {
		return fmt.Errorf("%s contains invalid characters (only alphanumeric, underscore, hyphen allowed): %q", name, value)
	}
	return nil
}

// ValidateSubjectField validates that a string is safe for use in a NATS subject
// field that may contain dots (e.g., "team.agent" in the Envelope To field).
// Dots are allowed as delimiters, but NATS wildcards (*, >), spaces, and other
// special characters are rejected. Each dot-separated segment must be non-empty.
var validSubjectField = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)

func ValidateSubjectField(name, value string) error {
	if value == "" {
		return fmt.Errorf("%s must not be empty", name)
	}
	if len(value) > MaxSubjectFieldLength {
		return fmt.Errorf("%s exceeds maximum length of %d bytes: %q", name, MaxSubjectFieldLength, value)
	}
	if !validSubjectField.MatchString(value) {
		return fmt.Errorf("%s contains invalid characters (only alphanumeric, underscore, hyphen, dot allowed): %q", name, value)
	}
	// Reject leading/trailing dots and consecutive dots (empty segments).
	if strings.HasPrefix(value, ".") || strings.HasSuffix(value, ".") || strings.Contains(value, "..") {
		return fmt.Errorf("%s contains empty subject segments: %q", name, value)
	}
	return nil
}

// ValidatePathComponent validates that a string is safe for use in file paths.
// Prevents path traversal attacks.
func ValidatePathComponent(name, value string) error {
	if err := ValidateSubjectComponent(name, value); err != nil {
		return err
	}
	// Defense-in-depth: ValidateSubjectComponent's regex already rejects "..", "/",
	// and "\" because those characters are not in [a-zA-Z0-9_-]. These explicit
	// checks remain as a safety net in case the regex is ever relaxed in the future.
	if strings.Contains(value, "..") || strings.Contains(value, "/") || strings.Contains(value, "\\") {
		return fmt.Errorf("%s contains path traversal characters: %q", name, value)
	}
	return nil
}
