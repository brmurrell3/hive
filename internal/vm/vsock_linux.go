// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build linux

package vm

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"
)

// VsockForwarder listens on the Firecracker vsock UDS path and forwards
// connections to a local TCP port (where the NATS server is listening).
//
// Firecracker's vsock implementation works as follows:
//   - The guest connects to AF_VSOCK CID 2 (host) on a specific port (e.g., 4222).
//   - Firecracker creates a connection on the host-side UDS with the port appended
//     as a suffix: "{uds_path}_{port}". For example, if the vsock UDS path is
//     "/tmp/vm.vsock" and the guest connects to port 4222, Firecracker connects
//     to "/tmp/vm.vsock_4222".
//   - The host must listen on that path to accept the connection.
//
// VsockForwarder listens on the Firecracker-generated UDS path and proxies
// each accepted connection to the local NATS server (or any TCP endpoint).
type VsockForwarder struct {
	udsPath    string // UDS path to listen on (e.g., "/path/to/firecracker.sock.vsock_4222")
	targetAddr string // TCP address to forward to (e.g., "127.0.0.1:4222")

	listener net.Listener
	logger   *slog.Logger
	wg       sync.WaitGroup
	cancel   context.CancelFunc

	// mu protects conns during concurrent access from acceptLoop and Stop.
	mu    sync.Mutex
	conns []net.Conn // tracked active connections for forced shutdown
}

// NewVsockForwarder creates a new VsockForwarder.
//
// vsockUDSPath is the base vsock UDS path configured in Firecracker (e.g.,
// "/path/to/firecracker.sock.vsock"). port is the guest-side vsock port that
// will be appended as a suffix. targetAddr is the local TCP address to forward
// connections to (e.g., "127.0.0.1:4222").
func NewVsockForwarder(vsockUDSPath string, port uint32, targetAddr string, logger *slog.Logger) *VsockForwarder {
	// Firecracker appends _{port} to the vsock UDS path.
	udsPath := fmt.Sprintf("%s_%d", vsockUDSPath, port)

	return &VsockForwarder{
		udsPath:    udsPath,
		targetAddr: targetAddr,
		logger:     logger.With("component", "vsock-forwarder", "uds_path", udsPath),
	}
}

// Start begins listening on the Firecracker vsock UDS path and accepting
// connections. Each accepted connection is forwarded to the target TCP address.
// Returns once the listener is bound.
func (f *VsockForwarder) Start(ctx context.Context) error {
	f.mu.Lock()
	if f.listener != nil {
		f.mu.Unlock()
		return fmt.Errorf("vsock forwarder already started")
	}

	ln, err := net.Listen("unix", f.udsPath)
	if err != nil {
		f.mu.Unlock()
		return fmt.Errorf("listening on vsock UDS %s: %w", f.udsPath, err)
	}
	f.listener = ln

	ctx, f.cancel = context.WithCancel(ctx)
	f.mu.Unlock()

	f.logger.Info("vsock forwarder started",
		"uds_path", f.udsPath,
		"target", f.targetAddr,
	)

	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		f.acceptLoop(ctx)
	}()

	return nil
}

// Stop shuts down the forwarder and waits for all active connections to drain.
// It closes all tracked connections with a short deadline to force io.Copy to
// return, preventing Stop from hanging on idle connections.
func (f *VsockForwarder) Stop() {
	f.logger.Info("stopping vsock forwarder")

	if f.cancel != nil {
		f.cancel()
	}

	if f.listener != nil {
		if err := f.listener.Close(); err != nil {
			f.logger.Debug("closing listener", "error", err)
		}
	}

	// Force all active connections to unblock by setting a short deadline.
	// This causes any blocked io.Copy to return with a deadline-exceeded error,
	// allowing the goroutines to exit and wg.Wait to complete.
	f.mu.Lock()
	for _, c := range f.conns {
		_ = c.SetDeadline(time.Now().Add(100 * time.Millisecond))
	}
	f.mu.Unlock()

	f.wg.Wait()

	// Clean up the Unix domain socket file to prevent "address already in use"
	// errors on agent restart.
	os.Remove(f.udsPath)

	f.logger.Info("vsock forwarder stopped")
}

// trackConn adds a connection to the tracked set.
func (f *VsockForwarder) trackConn(c net.Conn) {
	f.mu.Lock()
	f.conns = append(f.conns, c)
	f.mu.Unlock()
}

// untrackConn removes a connection from the tracked set.
func (f *VsockForwarder) untrackConn(c net.Conn) {
	f.mu.Lock()
	for i, tracked := range f.conns {
		if tracked == c {
			f.conns[i] = f.conns[len(f.conns)-1]
			f.conns[len(f.conns)-1] = nil
			f.conns = f.conns[:len(f.conns)-1]
			break
		}
	}
	f.mu.Unlock()
}

// UDSPath returns the UDS path this forwarder listens on.
func (f *VsockForwarder) UDSPath() string {
	return f.udsPath
}

// acceptLoop accepts connections on the UDS and spawns a goroutine to proxy each one.
// On transient accept errors, it uses exponential backoff (starting at 100ms,
// capped at 5s) instead of returning, which would leak this goroutine and
// orphan any io.Copy goroutines spawned by handleConn.
func (f *VsockForwarder) acceptLoop(ctx context.Context) {
	const (
		minBackoff = 100 * time.Millisecond
		maxBackoff = 5 * time.Second
	)
	backoff := minBackoff

	for {
		conn, err := f.listener.Accept()
		if err != nil {
			// Always check context first -- if cancelled, exit cleanly.
			select {
			case <-ctx.Done():
				return
			default:
			}
			f.logger.Warn("accept error on vsock UDS, retrying",
				"error", err,
				"backoff", backoff,
			)
			// Wait with exponential backoff before retrying.
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			// Increase backoff for next error, capped at maxBackoff.
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}

		// Successful accept resets the backoff.
		backoff = minBackoff

		// Track the connection before spawning the goroutine so that if
		// Stop() runs between goroutine start and handleConn execution,
		// the connection is already in the tracked set and will be
		// force-closed.
		f.trackConn(conn)

		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			f.handleConn(conn)
		}()
	}
}

// handleConn proxies a single UDS connection to the target TCP address.
func (f *VsockForwarder) handleConn(udsConn net.Conn) {
	defer udsConn.Close()
	// udsConn is already tracked by acceptLoop before this goroutine starts.
	defer f.untrackConn(udsConn)

	tcpConn, err := net.Dial("tcp", f.targetAddr)
	if err != nil {
		f.logger.Error("failed to dial target",
			"target", f.targetAddr,
			"error", err,
		)
		return
	}
	defer tcpConn.Close()
	f.trackConn(tcpConn)
	defer f.untrackConn(tcpConn)

	f.logger.Debug("forwarding vsock connection to NATS",
		"target", f.targetAddr,
	)

	// Bidirectional copy. When either direction finishes (EOF or error),
	// close the write side of the opposite connection to unblock the other
	// io.Copy, preventing a hang if one side stalls.
	done := make(chan struct{})

	go func() {
		defer close(done)
		_, _ = io.Copy(tcpConn, udsConn)
		// Signal EOF to the TCP side.
		if tc, ok := tcpConn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()

	_, _ = io.Copy(udsConn, tcpConn)
	// Close the UDS connection to unblock the goroutine's io.Copy if it
	// is still reading from udsConn. The deferred udsConn.Close() is
	// idempotent so the double-close is safe.
	udsConn.Close()
	<-done
}
