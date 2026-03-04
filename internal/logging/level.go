// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

// Package logging provides shared logging utilities for Hive binaries.
package logging

import (
	"log/slog"
	"strings"
)

// ParseLevel converts a log level string to slog.Level.
// Accepted values: debug, info, warn, error. Defaults to info.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
