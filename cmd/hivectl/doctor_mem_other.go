// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build !linux && !darwin

package main

// systemMemoryMB returns 0 on unsupported platforms.
func systemMemoryMB() int64 {
	return 0
}
