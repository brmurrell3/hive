// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package backend

// SystemMemoryMB returns the total physical system RAM in megabytes.
// It uses platform-specific methods:
//   - Linux:  parses /proc/meminfo for MemTotal
//   - macOS:  reads hw.memsize via syscall.Sysctl
//
// Returns 0 if detection fails so the caller can apply a fallback.
// Platform implementations are in sysmem_linux.go and sysmem_darwin.go.
