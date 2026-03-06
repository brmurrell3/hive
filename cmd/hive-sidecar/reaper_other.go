//go:build !linux

package main

func startReaper() {} // no-op on non-Linux
