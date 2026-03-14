// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package vm

import (
	"context"
	"fmt"
	"sync"
)

// MockHypervisor implements Hypervisor for testing without KVM.
// It simulates VM lifecycle operations in memory using fake PIDs.
type MockHypervisor struct {
	mu      sync.Mutex
	running map[string]int // socketPath -> fake PID
	nextPID int

	// Error injection fields for testing error paths.
	CreateErr  error
	StartErr   error
	StopErr    error
	DestroyErr error
}

// NewMockHypervisor creates a new MockHypervisor ready for use.
func NewMockHypervisor() *MockHypervisor {
	return &MockHypervisor{
		running: make(map[string]int),
		nextPID: 10000,
	}
}

// CreateVM simulates VM creation by recording the socket path.
// Returns a synthetic PID for the created VM process. The same PID will be
// used by StartVM so that callers see consistent PID values.
func (m *MockHypervisor) CreateVM(_ context.Context, cfg VMConfig) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.CreateErr != nil {
		return 0, m.CreateErr
	}

	if _, exists := m.running[cfg.SocketPath]; exists {
		return 0, fmt.Errorf("mock: VM already exists at socket %s", cfg.SocketPath)
	}

	// Assign a fake PID and store it so StartVM will use the same value.
	pid := m.nextPID
	m.nextPID++
	// Store the negative PID to indicate created-but-not-started.
	// StartVM flips it to positive to indicate running.
	m.running[cfg.SocketPath] = -pid
	return pid, nil
}

// StartVM simulates VM boot by marking the stored PID as running.
// FC-C2: Accepts context.Context to match the updated Hypervisor interface.
func (m *MockHypervisor) StartVM(_ context.Context, socketPath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.StartErr != nil {
		return m.StartErr
	}

	pid, exists := m.running[socketPath]
	if !exists {
		return fmt.Errorf("mock: no VM at socket %s", socketPath)
	}
	if pid > 0 {
		return fmt.Errorf("mock: VM at socket %s is already running with PID %d", socketPath, pid)
	}

	// Flip negative PID to positive to mark as running.
	m.running[socketPath] = -pid
	return nil
}

// StopVM simulates graceful VM shutdown.
func (m *MockHypervisor) StopVM(socketPath string, pid int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.StopErr != nil {
		return m.StopErr
	}

	storedPID, exists := m.running[socketPath]
	if !exists {
		return fmt.Errorf("mock: no VM at socket %s", socketPath)
	}

	// FC-C3: Verify PID matches to prevent stopping the wrong process.
	if storedPID != pid && storedPID != -pid {
		return fmt.Errorf("mock: PID mismatch for socket %s: stored %d, provided %d", socketPath, storedPID, pid)
	}

	delete(m.running, socketPath)
	return nil
}

// DestroyVM simulates forceful VM termination.
func (m *MockHypervisor) DestroyVM(socketPath string, pid int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.DestroyErr != nil {
		return m.DestroyErr
	}

	// FC-C3: Check existence and verify PID matches before destroying.
	storedPID, exists := m.running[socketPath]
	if !exists {
		return fmt.Errorf("mock: no VM at socket %s", socketPath)
	}

	if storedPID != pid && storedPID != -pid {
		return fmt.Errorf("mock: PID mismatch for socket %s: stored %d, provided %d", socketPath, storedPID, pid)
	}

	delete(m.running, socketPath)
	return nil
}

// IsRunning checks whether a fake PID is still tracked as running.
// Only positive PIDs are considered running (negative means created-but-not-started).
func (m *MockHypervisor) IsRunning(pid int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, p := range m.running {
		if p == pid && p > 0 {
			return true
		}
	}
	return false
}

// RunningCount returns the number of currently running VMs. Useful for test assertions.
// Only counts VMs with positive PIDs (started), not created-but-not-started (negative PIDs).
func (m *MockHypervisor) RunningCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	count := 0
	for _, pid := range m.running {
		if pid > 0 {
			count++
		}
	}
	return count
}

// PIDForSocket returns the fake PID for a given socket path, or 0 if not found.
func (m *MockHypervisor) PIDForSocket(socketPath string) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.running[socketPath]
}
