// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build linux

package sidecar

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// VsockProxy listens on a local TCP port and proxies connections to the host
// via virtio-vsock. This allows the NATS client (which speaks TCP) to reach
// the host's NATS server through Firecracker's vsock device without any
// modifications to the NATS client itself.
//
// Inside a Firecracker VM, TCP networking is not available. The only
// communication path to the host is virtio-vsock. The guest connects to
// CID 2 (VMADDR_CID_HOST) on a well-known port, and Firecracker forwards
// the connection to a Unix domain socket on the host side.
type VsockProxy struct {
	localAddr string // TCP listen address inside the VM (e.g., "127.0.0.1:4222")
	hostCID   uint32 // vsock CID of the host (typically 2 = VMADDR_CID_HOST)
	hostPort  uint32 // vsock port on the host to connect to

	listener net.Listener
	logger   *slog.Logger
	wg       sync.WaitGroup
	cancel   context.CancelFunc

	// mu protects conns during concurrent access from acceptLoop and Stop.
	mu    sync.Mutex
	conns []net.Conn // tracked active connections for forced shutdown
}

// NewVsockProxy creates a new VsockProxy that will listen on localAddr and
// forward connections to the host via vsock at the given CID and port.
func NewVsockProxy(localAddr string, hostCID, hostPort uint32, logger *slog.Logger) *VsockProxy {
	return &VsockProxy{
		localAddr: localAddr,
		hostCID:   hostCID,
		hostPort:  hostPort,
		logger:    logger.With("component", "vsock-proxy"),
	}
}

// Start begins listening on the local TCP address and accepting connections.
// Each accepted connection is proxied to the host via vsock. Start returns
// once the listener is bound, or an error if binding fails.
func (p *VsockProxy) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", p.localAddr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", p.localAddr, err)
	}
	p.listener = ln

	ctx, p.cancel = context.WithCancel(ctx)

	p.logger.Info("vsock proxy started",
		"local_addr", ln.Addr().String(),
		"host_cid", p.hostCID,
		"host_port", p.hostPort,
	)

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.acceptLoop(ctx)
	}()

	return nil
}

// Stop shuts down the proxy listener and waits for all active connections
// to finish copying. It closes all tracked connections with a short deadline
// to force io.Copy to return, preventing Stop from hanging on idle connections.
func (p *VsockProxy) Stop() {
	p.logger.Info("stopping vsock proxy")

	if p.cancel != nil {
		p.cancel()
	}

	if p.listener != nil {
		if err := p.listener.Close(); err != nil {
			p.logger.Debug("closing listener", "error", err)
		}
	}

	// Force all active connections to unblock by setting a short deadline.
	p.mu.Lock()
	for _, c := range p.conns {
		_ = c.SetDeadline(time.Now().Add(100 * time.Millisecond))
	}
	p.mu.Unlock()

	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		p.logger.Warn("vsock proxy stop timed out")
	}
	p.logger.Info("vsock proxy stopped")
}

// trackConn adds a connection to the tracked set.
func (p *VsockProxy) trackConn(c net.Conn) {
	p.mu.Lock()
	p.conns = append(p.conns, c)
	p.mu.Unlock()
}

// untrackConn removes a connection from the tracked set.
func (p *VsockProxy) untrackConn(c net.Conn) {
	p.mu.Lock()
	for i, tracked := range p.conns {
		if tracked == c {
			p.conns[i] = p.conns[len(p.conns)-1]
			p.conns[len(p.conns)-1] = nil
			p.conns = p.conns[:len(p.conns)-1]
			break
		}
	}
	p.mu.Unlock()
}

// acceptLoop accepts TCP connections and spawns a goroutine to proxy each one.
func (p *VsockProxy) acceptLoop(ctx context.Context) {
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			// Check if we were shut down.
			select {
			case <-ctx.Done():
				return
			default:
			}
			p.logger.Warn("accept error", "error", err)
			return
		}

		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.handleConn(ctx, conn)
		}()
	}
}

// handleConn proxies a single TCP connection to the host via vsock.
func (p *VsockProxy) handleConn(ctx context.Context, tcpConn net.Conn) {
	defer tcpConn.Close()
	p.trackConn(tcpConn)
	defer p.untrackConn(tcpConn)

	vsockConn, err := p.dialVsock()
	if err != nil {
		p.logger.Error("failed to dial vsock",
			"host_cid", p.hostCID,
			"host_port", p.hostPort,
			"error", err,
		)
		return
	}
	defer vsockConn.Close()
	p.trackConn(vsockConn)
	defer p.untrackConn(vsockConn)

	p.logger.Debug("proxying connection",
		"tcp_remote", tcpConn.RemoteAddr().String(),
		"vsock_cid", p.hostCID,
		"vsock_port", p.hostPort,
	)

	const idleTimeout = 10 * time.Minute
	_ = tcpConn.SetDeadline(time.Now().Add(idleTimeout))
	_ = vsockConn.SetDeadline(time.Now().Add(idleTimeout))

	// Bidirectional copy. When either direction finishes (or errors), we
	// close both connections so the other direction unblocks.
	done := make(chan struct{})

	go func() {
		defer close(done)
		copyWithDeadline(vsockConn, tcpConn, idleTimeout)
		// Shut down the write side of the vsock connection to signal EOF.
		if sc, ok := vsockConn.(shutdownWriter); ok {
			_ = sc.CloseWrite()
		}
	}()

	copyWithDeadline(tcpConn, vsockConn, idleTimeout)
	// Shut down the write side of the TCP connection to signal EOF.
	if tc, ok := tcpConn.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}

	<-done
}

// copyWithDeadline copies from src to dst, resetting the idle deadline on
// both connections after each successful read/write cycle.
func copyWithDeadline(dst net.Conn, src net.Conn, timeout time.Duration) {
	buf := make([]byte, 32*1024)
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return
			}
			_ = dst.SetDeadline(time.Now().Add(timeout))
			_ = src.SetDeadline(time.Now().Add(timeout))
		}
		if readErr != nil {
			return
		}
	}
}

// shutdownWriter is implemented by connections that support half-close.
type shutdownWriter interface {
	CloseWrite() error
}

// dialVsock creates a vsock connection to the host. It uses raw syscalls
// because Go's net package does not natively support AF_VSOCK.
func (p *VsockProxy) dialVsock() (net.Conn, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("creating vsock socket: %w", err)
	}

	sa := &unix.SockaddrVM{
		CID:  p.hostCID,
		Port: p.hostPort,
	}

	if err := unix.Connect(fd, sa); err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("connecting vsock to CID %d port %d: %w", p.hostCID, p.hostPort, err)
	}

	// Wrap the raw fd into a net.Conn via os.File -> net.FileConn.
	file := os.NewFile(uintptr(fd), fmt.Sprintf("vsock:%d:%d", p.hostCID, p.hostPort))
	if file == nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("os.NewFile returned nil for vsock fd %d", fd)
	}

	conn, err := net.FileConn(file)
	// FileConn dups the fd, so close the original file.
	file.Close()
	if err != nil {
		return nil, fmt.Errorf("converting vsock fd to net.Conn: %w", err)
	}

	return conn, nil
}

// Addr returns the address the proxy is listening on, or empty string if not started.
func (p *VsockProxy) Addr() string {
	if p.listener != nil {
		return p.listener.Addr().String()
	}
	return ""
}
