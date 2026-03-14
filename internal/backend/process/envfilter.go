// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package process

import "strings"

// isEnvVarDenied returns true if the given environment variable key is on the
// denylist. The denylist prevents injection of framework-critical (HIVE_*),
// dynamic linker (LD_*, DYLD_*), and shell-critical (PATH, HOME, SHELL)
// variables via model configuration.
func isEnvVarDenied(key string) bool {
	upper := strings.ToUpper(key)
	return strings.HasPrefix(upper, "HIVE_") ||
		strings.HasPrefix(upper, "LD_") ||
		strings.HasPrefix(upper, "DYLD_") ||
		upper == "PATH" || upper == "HOME" || upper == "SHELL"
}

// filterModelEnv returns a copy of env with dangerous keys removed.
// BE-H1: The denylist (HIVE_*, LD_*, DYLD_*, PATH, HOME, SHELL) is applied
// to prevent injection via model configuration.
func filterModelEnv(env map[string]string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	filtered := make(map[string]string, len(env))
	for k, v := range env {
		if isEnvVarDenied(k) {
			continue
		}
		filtered[k] = v
	}
	if len(filtered) == 0 {
		return nil
	}
	return filtered
}
