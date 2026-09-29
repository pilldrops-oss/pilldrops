//go:build linux

package main

import (
	"errors"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// These tests only pass on a KVM host: Docker's seccomp blocks AF_VSOCK
// (EPERM), and a Firecracker guest has no vsock_loopback so it cannot dial
// itself (ENODEV). Skips are skips, not passes.

// vsockTestPort stays clear of envd's 6001/6002 in case this runs inside a sandbox.
const vsockTestPort = 21000

// requireVsock returns a listener, skipping only when the machine has no
// AF_VSOCK transport. Availability is probed with a raw bind, not listenVsock,
// so a regression in listenVsock fails instead of skipping.
func requireVsock(t *testing.T, port int) net.Listener {
	t.Helper()
	probe := rawListenVsock(t, port+100) // skips here, and only here
	unix.Close(probe)

	l, err := listenVsock(port)
	if err != nil {
		t.Fatalf("AF_VSOCK works on this machine — a raw bind+listen on port %d succeeded — "+
			"but listenVsock failed: %v", port+100, err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func TestListenVsockFailureIsReportable(t *testing.T) {
	l, err := listenVsock(vsockTestPort)
	if err == nil {
		l.Close()
		t.Skip("SKIP (not a pass): AF_VSOCK works here, so there is no failure to describe")
	}
	if !strings.Contains(err.Error(), "vsock") {
		t.Fatalf("listenVsock error %q does not name vsock; the transport notice built from it "+
			"would not tell an operator what was unavailable", err)
	}
}

// A payload must be read back, not merely a connection the kernel completed.
// Bypasses serveCredChannel so the result does not depend on the loopback peer CID.
func TestVsockListenerAcceptsAConnection(t *testing.T) {
	l := requireVsock(t, vsockTestPort)

	cid, err := localVsockCID()
	if err != nil {
		t.Skipf("SKIP (not a pass): no local vsock CID: %v", err)
	}

	// Dial before parking an accept goroutine, so a guest skips instead of hanging.
	c := requireLocalDial(t, cid, vsockTestPort)
	defer c.Close()

	const payload = "GUEST-WROTE-THIS\n"
	done := make(chan error, 1)
	go func() {
		a, aerr := l.Accept()
		if aerr != nil {
			done <- aerr
			return
		}
		defer a.Close()
		_ = a.SetDeadline(time.Now().Add(5 * time.Second))
		_, werr := a.Write([]byte(payload))
		done <- werr
	}()

	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	if err != nil && n == 0 {
		t.Fatalf("read: %v — the connection may have been completed by the KERNEL "+
			"while no userspace process accepted it; that is the failure this test exists for", err)
	}
	if got := string(buf[:n]); got != payload {
		t.Fatalf("read %q, want %q", got, payload)
	}
	if werr := <-done; werr != nil {
		t.Fatalf("server side: %v", werr)
	}
}

// The expected answer is derived from where this runs: a push succeeds exactly
// when the local CID is the host's, so a guest tenant cannot rotate its own credential.
func TestVsockPeerPolicyOverARealTransport(t *testing.T) {
	l := requireVsock(t, vsockTestPort+4)
	hashes := newHashStore("")
	go serveCredChannel(l, hashes)

	cid, err := localVsockCID()
	if err != nil {
		t.Skipf("SKIP (not a pass): no local vsock CID: %v", err)
	}
	wantAllowed := cid == unix.VMADDR_CID_HOST

	hash := strings.Repeat("ab", 32)
	reply, err := vsockPush(cid, vsockTestPort+4, credSetHash+" "+hash)
	if err != nil {
		t.Skipf("SKIP (not a pass): this kernel cannot dial its own vsock CID %d: %v", cid, err)
	}
	gotAllowed := reply == "OK"
	if gotAllowed != wantAllowed {
		t.Fatalf("local CID %d: push reply %q (allowed=%v), want allowed=%v",
			cid, reply, gotAllowed, wantAllowed)
	}
	if stored := hashes.Get(); (stored == hash) != wantAllowed {
		t.Fatalf("local CID %d: stored=%q but allowed=%v — the reply and the store disagree",
			cid, stored, wantAllowed)
	}
}

func TestVsockNoListenerControl(t *testing.T) {
	unix.Close(rawListenVsock(t, vsockTestPort+1))
	cid, cidErr := localVsockCID()
	if cidErr != nil {
		t.Skipf("SKIP (not a pass): no local vsock CID: %v", cidErr)
	}

	// In a guest every local dial fails, so first prove one can succeed here.
	live := requireVsock(t, vsockTestPort+5)
	requireLocalDial(t, cid, vsockTestPort+5).Close()
	live.Close()

	if _, err := vsockPush(cid, vsockTestPort+1, credSetHash+" x"); err == nil {
		t.Fatal("connecting to a vsock port with NO listener succeeded; " +
			"this control is what makes the positive test mean something")
	}
}

// syscall.Accept4 fails with EAFNOSUPPORT on a vsock peer; unix.Accept4 parses it.
func TestVsockAcceptUsesTheSockaddrAwarePackage(t *testing.T) {
	l := requireVsock(t, vsockTestPort+2)
	cid, err := localVsockCID()
	if err != nil {
		t.Skipf("SKIP (not a pass): no local vsock CID: %v", err)
	}

	// Our listener parses the peer address. Dial before accepting so a guest skips instead of parking.
	client := requireLocalDial(t, cid, vsockTestPort+2)
	defer client.Close()
	conn, err := l.Accept()
	if err != nil {
		t.Fatalf("Accept: %v — TRAP 2 (EAFNOSUPPORT from a sockaddr parser that "+
			"does not know AF_VSOCK) looks exactly like this", err)
	}
	defer conn.Close()
	if conn.RemoteAddr().Network() != "vsock" {
		t.Fatalf("RemoteAddr network = %q, want vsock", conn.RemoteAddr().Network())
	}
	if strings.Contains(conn.RemoteAddr().String(), "any") {
		t.Fatal("the accepted peer address was not parsed; unix.Accept4 stopped " +
			"understanding AF_VSOCK and TRAP 2 is back")
	}

	// The stdlib path must still fail, or the choice above is incidental.
	rawFD := rawListenVsock(t, vsockTestPort+3)
	defer unix.Close(rawFD)
	client2 := requireLocalDial(t, cid, vsockTestPort+3)
	defer client2.Close()
	_, _, serr := syscall.Accept4(rawFD, 0)
	if serr == nil {
		t.Skip("SKIP (not a pass): the stdlib syscall package now parses AF_VSOCK peers too. " +
			"That is good news; vsock.go's comment about TRAP 2 needs updating.")
	}
	if !errors.Is(serr, unix.EAFNOSUPPORT) {
		t.Logf("note: stdlib accept failed with %v rather than EAFNOSUPPORT", serr)
	}
}

// rawListenVsock is a bare bind+listen without os.NewFile, so the fd stays
// blocking and can be handed to the stdlib accept.
func rawListenVsock(t *testing.T, port int) int {
	t.Helper()
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Skipf("SKIP (not a pass): AF_VSOCK socket: %v", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: uint32(port)}); err != nil {
		unix.Close(fd)
		t.Skipf("SKIP (not a pass): AF_VSOCK bind: %v", err)
	}
	if err := unix.Listen(fd, 4); err != nil {
		unix.Close(fd)
		t.Skipf("SKIP (not a pass): AF_VSOCK listen: %v", err)
	}
	return fd
}

// requireLocalDial skips when this kernel cannot dial its own vsock CID (a guest without vsock_loopback).
func requireLocalDial(t *testing.T, cid uint32, port int) net.Conn {
	t.Helper()
	c, err := dialVsockTest(cid, port)
	if err != nil {
		t.Skipf("SKIP (not a pass): this kernel cannot dial its own vsock CID %d: %v. "+
			"The listener is still exercised from the host in the live Firecracker run.", cid, err)
	}
	return c
}

// dialVsockTest connects to a vsock port on this machine; envd itself never dials vsock.
func dialVsockTest(cid uint32, port int) (net.Conn, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if err := unix.Connect(fd, &unix.SockaddrVM{CID: cid, Port: uint32(port)}); err != nil {
		unix.Close(fd)
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "vsock-test-dial")
	return &vsockConn{f: f, local: vsockAddr{cid: cid}, remote: vsockAddr{cid: cid, port: uint32(port)}}, nil
}

func vsockPush(cid uint32, port int, command string) (string, error) {
	c, err := dialVsockTest(cid, port)
	if err != nil {
		return "", err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte(command + "\n")); err != nil {
		return "", err
	}
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	if n == 0 && err != nil {
		return "", err
	}
	return strings.TrimSpace(string(buf[:n])), nil
}
