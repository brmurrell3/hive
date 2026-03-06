//go:build linux

package main

import (
	"syscall"
	"time"
)

func startReaper() {
	go func() {
		for {
			for {
				var ws syscall.WaitStatus
				pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
				if pid <= 0 || err != nil {
					break
				}
			}
			time.Sleep(1 * time.Second)
		}
	}()
}
