//go:build !linux

package vm

import (
	"context"
	"fmt"
	"log/slog"
)

// VsockForwarder is a stub for non-Linux platforms. Firecracker and vsock
// are Linux-only, so on other platforms this forwarder cannot operate.
type VsockForwarder struct {
	udsPath string
	logger  *slog.Logger
}

// NewVsockForwarder creates a stub VsockForwarder on non-Linux platforms.
func NewVsockForwarder(vsockUDSPath string, port uint32, targetAddr string, logger *slog.Logger) *VsockForwarder {
	return &VsockForwarder{
		udsPath: fmt.Sprintf("%s_%d", vsockUDSPath, port),
		logger:  logger.With("component", "vsock-forwarder"),
	}
}

// Start returns an error on non-Linux platforms since vsock is not available.
func (f *VsockForwarder) Start(_ context.Context) error {
	return fmt.Errorf("vsock forwarder is not supported on this platform (requires Linux)")
}

// Stop is a no-op on non-Linux platforms.
func (f *VsockForwarder) Stop() {}

// UDSPath returns the UDS path this forwarder would listen on.
func (f *VsockForwarder) UDSPath() string {
	return f.udsPath
}
