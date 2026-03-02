// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build linux

package main

import (
	"syscall"
	"time"
)

func startReaper(stop <-chan struct{}) {
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			for {
				var ws syscall.WaitStatus
				pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
				if pid <= 0 || err != nil {
					break
				}
			}
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
		}
	}()
}
