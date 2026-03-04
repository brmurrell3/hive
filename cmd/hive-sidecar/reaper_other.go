// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build !linux

package main

func startReaper(_ <-chan struct{}) {} // no-op on non-Linux
