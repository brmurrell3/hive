//go:build linux

package vm

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
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
	udsPath   string // UDS path to listen on (e.g., "/path/to/firecracker.sock.vsock_4222")
	targetAddr string // TCP address to forward to (e.g., "127.0.0.1:4222")

	listener net.Listener
	logger   *slog.Logger
	wg       sync.WaitGroup
	cancel   context.CancelFunc
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
	ln, err := net.Listen("unix", f.udsPath)
	if err != nil {
		return fmt.Errorf("listening on vsock UDS %s: %w", f.udsPath, err)
	}
	f.listener = ln

	ctx, f.cancel = context.WithCancel(ctx)

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
func (f *VsockForwarder) Stop() {
	f.logger.Info("stopping vsock forwarder")

	if f.cancel != nil {
		f.cancel()
	}

	if f.listener != nil {
		f.listener.Close()
	}

	f.wg.Wait()
	f.logger.Info("vsock forwarder stopped")
}

// UDSPath returns the UDS path this forwarder listens on.
func (f *VsockForwarder) UDSPath() string {
	return f.udsPath
}

// acceptLoop accepts connections on the UDS and spawns a goroutine to proxy each one.
func (f *VsockForwarder) acceptLoop(ctx context.Context) {
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			f.logger.Warn("accept error on vsock UDS", "error", err)
			return
		}

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

	tcpConn, err := net.Dial("tcp", f.targetAddr)
	if err != nil {
		f.logger.Error("failed to dial target",
			"target", f.targetAddr,
			"error", err,
		)
		return
	}
	defer tcpConn.Close()

	f.logger.Debug("forwarding vsock connection to NATS",
		"target", f.targetAddr,
	)

	// Bidirectional copy.
	done := make(chan struct{})

	go func() {
		defer close(done)
		io.Copy(tcpConn, udsConn)
		// Signal EOF to the TCP side.
		if tc, ok := tcpConn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()

	io.Copy(udsConn, tcpConn)
	<-done
}
