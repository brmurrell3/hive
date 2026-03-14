// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build !linux && !darwin

package backend

// SystemMemoryMB returns 0 on unsupported platforms so the caller can apply a fallback.
func SystemMemoryMB() int64 {
	return 0
}
