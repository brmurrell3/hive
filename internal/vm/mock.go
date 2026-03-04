package vm

import (
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
func (m *MockHypervisor) CreateVM(cfg VMConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.CreateErr != nil {
		return m.CreateErr
	}

	if _, exists := m.running[cfg.SocketPath]; exists {
		return fmt.Errorf("mock: VM already exists at socket %s", cfg.SocketPath)
	}

	// Assign a fake PID but don't mark as running yet (StartVM does that).
	// Store with PID 0 to indicate created but not started.
	m.running[cfg.SocketPath] = 0
	return nil
}

// StartVM simulates VM boot by assigning a fake PID.
func (m *MockHypervisor) StartVM(socketPath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.StartErr != nil {
		return m.StartErr
	}

	pid, exists := m.running[socketPath]
	if !exists {
		return fmt.Errorf("mock: no VM at socket %s", socketPath)
	}
	if pid != 0 {
		return fmt.Errorf("mock: VM at socket %s is already running with PID %d", socketPath, pid)
	}

	m.running[socketPath] = m.nextPID
	m.nextPID++
	return nil
}

// StopVM simulates graceful VM shutdown.
func (m *MockHypervisor) StopVM(socketPath string, pid int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.StopErr != nil {
		return m.StopErr
	}

	if _, exists := m.running[socketPath]; !exists {
		return fmt.Errorf("mock: no VM at socket %s", socketPath)
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

	delete(m.running, socketPath)
	return nil
}

// IsRunning checks whether a fake PID is still tracked as running.
func (m *MockHypervisor) IsRunning(pid int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, p := range m.running {
		if p == pid && p != 0 {
			return true
		}
	}
	return false
}

// RunningCount returns the number of currently running VMs. Useful for test assertions.
func (m *MockHypervisor) RunningCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	count := 0
	for _, pid := range m.running {
		if pid != 0 {
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
