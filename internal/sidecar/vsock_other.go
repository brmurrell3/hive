//go:build !linux

package sidecar

import (
	"context"
	"fmt"
	"log/slog"
)

// VsockProxy is a stub for non-Linux platforms. Virtio-vsock is a Linux-only
// feature, so on other platforms (macOS, Windows) this proxy cannot operate.
// All methods return errors indicating that vsock is not supported.
type VsockProxy struct {
	logger *slog.Logger
}

// NewVsockProxy creates a stub VsockProxy on non-Linux platforms.
func NewVsockProxy(localAddr string, hostCID, hostPort uint32, logger *slog.Logger) *VsockProxy {
	return &VsockProxy{
		logger: logger.With("component", "vsock-proxy"),
	}
}

// Start returns an error on non-Linux platforms since vsock is not available.
func (p *VsockProxy) Start(_ context.Context) error {
	return fmt.Errorf("vsock proxy is not supported on this platform (requires Linux)")
}

// Stop is a no-op on non-Linux platforms.
func (p *VsockProxy) Stop() {}

// Addr returns empty string on non-Linux platforms.
func (p *VsockProxy) Addr() string {
	return ""
}
