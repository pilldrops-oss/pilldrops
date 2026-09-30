//go:build linux

package main

// A hand-rolled AF_VSOCK net.Listener. net.FileListener rejects AF_VSOCK fds,
// and syscall.Accept fails with EAFNOSUPPORT parsing a vsock peer, so use
// unix.Accept4 (which knows AF_VSOCK) inside RawConn.Read on a poller-owned fd.

import (
	"errors"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// listenVsock listens on an AF_VSOCK port bound to VMADDR_CID_ANY.
// Callers must log the error rather than discard it.
func listenVsock(port int) (net.Listener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}
	sa := &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: uint32(port)}
	if err := unix.Bind(fd, sa); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("vsock bind port %d: %w", port, err)
	}
	if err := unix.Listen(fd, 16); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("vsock listen port %d: %w", port, err)
	}

	// os.NewFile sets O_NONBLOCK and hands the fd to the poller; Accept relies on that.
	f := os.NewFile(uintptr(fd), fmt.Sprintf("vsock:%d", port))
	rc, err := f.SyscallConn()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("vsock raw conn: %w", err)
	}
	return &vsockListener{f: f, rc: rc, port: uint32(port)}, nil
}

// localVsockCID returns this machine's vsock context id; a guest answers 3, a host VMADDR_CID_HOST.
func localVsockCID() (uint32, error) {
	f, err := os.Open("/dev/vsock")
	if err != nil {
		return 0, fmt.Errorf("open /dev/vsock: %w", err)
	}
	defer func() { _ = f.Close() }()
	cid, err := unix.IoctlGetInt(int(f.Fd()), unix.IOCTL_VM_SOCKETS_GET_LOCAL_CID)
	if err != nil {
		return 0, fmt.Errorf("get local vsock cid: %w", err)
	}
	return uint32(cid), nil
}

type vsockListener struct {
	f    *os.File
	rc   syscall.RawConn
	port uint32
	// RawConn.Read does not wrap the poller's closed-fd error into os.ErrClosed,
	// so track Close ourselves or serveCredChannel retries forever after shutdown.
	closed atomic.Bool
}

// Accept waits for and returns the next connection. Returning false from the
// closure parks on the poller, so EAGAIN is a wait and Close aborts it.
func (l *vsockListener) Accept() (net.Conn, error) {
	var (
		nfd  int
		peer unix.Sockaddr
		aerr error
	)
	err := l.rc.Read(func(fd uintptr) bool {
		for {
			n, sa, e := unix.Accept4(int(fd), unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK)
			switch {
			case e == nil:
				nfd, peer = n, sa
				return true
			case errors.Is(e, unix.EINTR):
				continue
			case errors.Is(e, unix.EAGAIN):
				return false // not ready; park and retry
			default:
				aerr = e
				return true
			}
		}
	})
	if err != nil || aerr != nil {
		e := err
		if e == nil {
			e = aerr
		}
		// Normalise shutdown to net.ErrClosed so serveCredChannel ends quietly.
		if l.closed.Load() || errors.Is(e, os.ErrClosed) {
			return nil, l.opErr("accept", net.ErrClosed)
		}
		return nil, l.opErr("accept", e)
	}
	return newVsockConn(nfd, l.Addr(), vsockAddrFrom(peer)), nil
}

func (l *vsockListener) Close() error {
	l.closed.Store(true)
	return l.f.Close()
}

func (l *vsockListener) Addr() net.Addr { return vsockAddr{cid: unix.VMADDR_CID_ANY, port: l.port} }

func (l *vsockListener) opErr(op string, err error) error {
	return &net.OpError{Op: op, Net: "vsock", Addr: l.Addr(), Err: err}
}

// vsockConn is a net.Conn over an accepted AF_VSOCK fd. Delegating to *os.File
// is what makes deadlines and Close-interrupts-Read work.
type vsockConn struct {
	f             *os.File
	local, remote net.Addr
}

func newVsockConn(fd int, local, remote net.Addr) net.Conn {
	return &vsockConn{
		f:      os.NewFile(uintptr(fd), "vsock-conn"),
		local:  local,
		remote: remote,
	}
}

func (c *vsockConn) Read(b []byte) (int, error)  { return c.f.Read(b) }
func (c *vsockConn) Write(b []byte) (int, error) { return c.f.Write(b) }
func (c *vsockConn) Close() error                { return c.f.Close() }
func (c *vsockConn) LocalAddr() net.Addr         { return c.local }
func (c *vsockConn) RemoteAddr() net.Addr        { return c.remote }

func (c *vsockConn) SetDeadline(t time.Time) error      { return c.f.SetDeadline(t) }
func (c *vsockConn) SetReadDeadline(t time.Time) error  { return c.f.SetReadDeadline(t) }
func (c *vsockConn) SetWriteDeadline(t time.Time) error { return c.f.SetWriteDeadline(t) }

type vsockAddr struct{ cid, port uint32 }

func (a vsockAddr) Network() string { return "vsock" }

// ContextID satisfies credPeer so serveCredConn can tell a host push from a tenant's loopback connection.
func (a vsockAddr) ContextID() uint32 { return a.cid }

func (a vsockAddr) String() string {
	if a.cid == unix.VMADDR_CID_ANY {
		return fmt.Sprintf("vsock(any):%d", a.port)
	}
	return fmt.Sprintf("vsock(%d):%d", a.cid, a.port)
}

func vsockAddrFrom(sa unix.Sockaddr) net.Addr {
	vm, ok := sa.(*unix.SockaddrVM)
	if !ok {
		// Only reachable if unix.Accept4 stops parsing AF_VSOCK peers; a named addr shows up in logs.
		return vsockAddr{cid: unix.VMADDR_CID_ANY, port: 0}
	}
	return vsockAddr{cid: vm.CID, port: vm.Port}
}
