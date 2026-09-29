//go:build linux

package vm

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Everything here is a UNIX socket and a directory; the tests that need a VM
// are in credpush_live_test.go.

const testHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// A restored VM's socket keeps the snapshotted device's name while the chroot
// is named for the new jailer id.
func TestVsockUDSPathFindsASocketNamedForADifferentID(t *testing.T) {
	root := t.TempDir()
	runDir := filepath.Join(root, "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Named for the VM the snapshot was taken from.
	other := filepath.Join(runDir, "sb-1756800000000-501da70c.vsock")
	touch(t, other)

	got, err := vsockUDSPath(root, "sb-1756800000000-2d0520de")
	if err != nil {
		t.Fatalf("vsockUDSPath: %v — this is the failure that reads as "+
			"'the guest is not listening' and is really a path built by hand", err)
	}
	if got != other {
		t.Fatalf("got %q, want %q", got, other)
	}
}

func TestVsockUDSPathReportsAnEmptyChroot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "run"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := vsockUDSPath(root, "sb-x")
	if !errors.Is(err, ErrVsockUDSNotFound) {
		t.Fatalf("err = %v, want ErrVsockUDSNotFound", err)
	}
	if !strings.Contains(err.Error(), filepath.Join(root, "run")) {
		t.Errorf("error %q does not name the directory it looked in", err)
	}
}

// The wrong socket would hand this sandbox's credential to another VM's guest.
func TestVsockUDSPathRefusesToGuessBetweenSockets(t *testing.T) {
	root := t.TempDir()
	runDir := filepath.Join(root, "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	touch(t, filepath.Join(runDir, "a.vsock"))
	touch(t, filepath.Join(runDir, "b.vsock"))

	if _, err := vsockUDSPath(root, "c"); !errors.Is(err, ErrVsockUDSNotFound) {
		t.Fatalf("err = %v, want a refusal when two sockets are candidates", err)
	}
	// ...but an exact match by sandbox id is not ambiguous.
	mine := filepath.Join(runDir, "c.vsock")
	touch(t, mine)
	got, err := vsockUDSPath(root, "c")
	if err != nil || got != mine {
		t.Fatalf("got (%q, %v), want %q", got, err, mine)
	}
}

func TestVsockUDSPathFallsBackToTheJailerBaseDir(t *testing.T) {
	base := t.TempDir()
	old := jailerBaseDir
	jailerBaseDir = base
	t.Cleanup(func() { jailerBaseDir = old })

	runDir := filepath.Join(base, "firecracker", "sb-9", "root", "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(runDir, "sb-9.vsock")
	touch(t, want)

	got, err := vsockUDSPath("", "sb-9")
	if err != nil || got != want {
		t.Fatalf("got (%q, %v), want %q", got, err, want)
	}
}

// Pins the exact bytes envd's guest half was written against.
func TestPushCredentialSpeaksTheWireFormatSection1Fixed(t *testing.T) {
	srv := startFakeVMSocket(t, fakeVM{connectAck: "OK 1073741824", setHashReply: "OK"})

	if err := pushCredential(srv.path, credVsockPort, testHash); err != nil {
		t.Fatalf("pushCredential: %v", err)
	}
	got := srv.received()
	want := []string{
		fmt.Sprintf("CONNECT %d", credVsockPort),
		"SETHASH " + testHash,
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("guest saw %q, want %q", got, want)
	}
}

// The fake acks CONNECT and never answers SETHASH; success here would be a
// push that silently delivers nothing.
func TestPushCredentialRequiresEnvdsReplyNotFirecrackersAck(t *testing.T) {
	srv := startFakeVMSocket(t, fakeVM{connectAck: "OK 1073741824", setHashReply: ""})

	err := pushCredential(srv.path, credVsockPort, testHash)
	if err == nil {
		t.Fatal("pushCredential reported success on Firecracker's CONNECT ack alone; " +
			"that ack proves the vsock device works and says nothing about envd")
	}
	if !errors.Is(err, ErrCredentialPush) {
		t.Errorf("err = %v, want it to wrap ErrCredentialPush", err)
	}
	// The message must say which half failed.
	if !strings.Contains(err.Error(), "no reply") && !strings.Contains(err.Error(), "nothing in the guest accepted") {
		t.Errorf("error %q does not distinguish 'the device worked' from 'envd answered'", err)
	}
}

// An ERR reply is reported verbatim, not retried.
func TestPushCredentialSurfacesEnvdsRefusal(t *testing.T) {
	srv := startFakeVMSocket(t, fakeVM{
		connectAck:   "OK 1073741824",
		setHashReply: "ERR only the host may push a credential",
	})

	err := pushCredential(srv.path, credVsockPort, testHash)
	if err == nil {
		t.Fatal("an ERR reply was treated as success")
	}
	if !strings.Contains(err.Error(), "only the host may push a credential") {
		t.Errorf("error %q drops envd's stated reason", err)
	}
}

func TestPushCredentialRefusesWhenFirecrackerRejectsCONNECT(t *testing.T) {
	srv := startFakeVMSocket(t, fakeVM{connectAck: "ERROR No such device or address"})

	err := pushCredential(srv.path, credVsockPort, testHash)
	if err == nil {
		t.Fatal("a rejected CONNECT was treated as success")
	}
	if !strings.Contains(err.Error(), "refused CONNECT") {
		t.Errorf("error %q does not say the handshake was refused", err)
	}
}

func TestPushCredentialReportsAMissingSocket(t *testing.T) {
	err := pushCredential(filepath.Join(t.TempDir(), "nope.vsock"), credVsockPort, testHash)
	if !errors.Is(err, ErrCredentialPush) {
		t.Fatalf("err = %v, want ErrCredentialPush", err)
	}
}

func TestDeliverCredentialRefusesAnEmptyHash(t *testing.T) {
	err := deliverCredential(t.TempDir(), "sb-1", "", 10*time.Millisecond)
	if !errors.Is(err, ErrCredentialPush) {
		t.Fatalf("err = %v, want ErrCredentialPush", err)
	}
	if !strings.Contains(err.Error(), "no token hash") {
		t.Errorf("error %q does not name the missing hash", err)
	}
}

func TestDeliverCredentialRetriesThenGivesUpNamingTheLastFailure(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "run"), 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err := deliverCredential(root, "sb-1", testHash, 300*time.Millisecond)
	if err == nil {
		t.Fatal("want an error when the socket never appears")
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Errorf("gave up after %v; it did not retry", elapsed)
	}
	if !errors.Is(err, ErrVsockUDSNotFound) {
		t.Errorf("err = %v, want the last failure (ErrVsockUDSNotFound)", err)
	}
}

// Positive control for the retry test.
func TestDeliverCredentialSucceedsOnceTheSocketAppears(t *testing.T) {
	root := t.TempDir()
	runDir := filepath.Join(root, "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		startFakeVMSocketAt(t, filepath.Join(runDir, "pool-deadbeef.vsock"),
			fakeVM{connectAck: "OK 1", setHashReply: "OK"})
	}()
	if err := deliverCredential(root, "sb-1", testHash, 5*time.Second); err != nil {
		t.Fatalf("deliverCredential: %v", err)
	}
}

// The two constants live in different binaries and cannot share a declaration.
func TestCredVsockPortMatchesEnvd(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "cmd", "envd", "main.go"))
	if err != nil {
		t.Fatalf("read envd main.go: %v", err)
	}
	want := fmt.Sprintf("credVsockPort = %d", credVsockPort)
	if !strings.Contains(string(b), want) {
		t.Fatalf("cmd/envd/main.go does not declare %q; the guest binds a port this node does not dial", want)
	}
}

// A guest that accepts the connection and answers nothing is stalled, not gone,
// and the credential push waits it out; a guest that never accepts keeps the
// short deadline. That distinction is the whole of why a five-minute-old
// sandbox stays recoverable: its stall is longer than credPushDeadline every
// time, and the same snapshot stalls the same length on every retry, so a
// single fixed deadline failed it repeatably rather than transiently.
func TestPushWaitsOutAStalledGuestButNotAnAbsentOne(t *testing.T) {
	const (
		short   = 300 * time.Millisecond
		stalled = 2 * time.Second
	)

	t.Run("accepted but unanswered is waited out", func(t *testing.T) {
		root := t.TempDir()
		runDir := filepath.Join(root, "run")
		if err := os.MkdirAll(runDir, 0o755); err != nil {
			t.Fatal(err)
		}
		startFakeVMSocketAt(t, filepath.Join(runDir, "sb-1.vsock"),
			fakeVM{connectAck: "OK 1", setHashReply: ""})

		start := time.Now()
		err := pushToGuestWaitingOutStalls(root, "sb-1", credSetHashCommand+" "+testHash, short, stalled)
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("a guest that never replies must still fail in the end")
		}
		if !errors.Is(err, ErrGuestNoReply) {
			t.Fatalf("err = %v, want it to name the stall (ErrGuestNoReply)", err)
		}
		if elapsed < stalled {
			t.Fatalf("gave up after %v: a guest that accepted the connection is alive and must be "+
				"waited out to %v, or a stalled restore fails forever", elapsed, stalled)
		}
	})

	t.Run("never accepted keeps the short deadline", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "run"), 0o755); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		err := pushToGuestWaitingOutStalls(root, "sb-1", credSetHashCommand+" "+testHash, short, stalled)
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("a guest with no vsock socket must fail")
		}
		if errors.Is(err, ErrGuestNoReply) {
			t.Fatalf("nothing ever accepted, so this is not a stall: %v", err)
		}
		if elapsed >= stalled {
			t.Fatalf("waited %v on a guest that never accepted; the stall deadline must not apply "+
				"to one that is simply not there", elapsed)
		}
	})
}

func touch(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
}

// fakeVM stands in for Firecracker's vsock UDS plus the guest behind it.
type fakeVM struct {
	connectAck   string // "" = say nothing and close
	setHashReply string // "" = say nothing and close
}

type fakeVMSocket struct {
	path string
	mu   sync.Mutex
	got  []string
}

func (s *fakeVMSocket) received() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.got...)
}

func startFakeVMSocket(t *testing.T, vm fakeVM) *fakeVMSocket {
	t.Helper()
	return startFakeVMSocketAt(t, filepath.Join(t.TempDir(), "v.vsock"), vm)
}

func startFakeVMSocketAt(t *testing.T, path string, vm fakeVM) *fakeVMSocket {
	t.Helper()
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	t.Cleanup(func() { _ = l.Close() })

	s := &fakeVMSocket{path: path}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				br := bufio.NewReader(c)

				line, err := br.ReadString('\n')
				if err != nil {
					return
				}
				s.mu.Lock()
				s.got = append(s.got, strings.TrimSpace(line))
				s.mu.Unlock()
				if vm.connectAck == "" {
					return
				}
				if _, err := c.Write([]byte(vm.connectAck + "\n")); err != nil {
					return
				}
				if !strings.HasPrefix(vm.connectAck, "OK") {
					return
				}

				line, err = br.ReadString('\n')
				if err != nil {
					return
				}
				s.mu.Lock()
				s.got = append(s.got, strings.TrimSpace(line))
				s.mu.Unlock()
				if vm.setHashReply == "" {
					return // connection completed, nothing in the guest answered
				}
				_, _ = c.Write([]byte(vm.setHashReply + "\n"))
			}()
		}
	}()
	return s
}

// The credential push must outlast the guest-side stall that caused the
// intermittent resume failure: a restored guest's Go runtime was measured
// leaving its accept handler runnable, unscheduled, before recovering on its
// own. A deadline under that turns a healthy sandbox into a failed resume,
// which is exactly what the old 15 s value did.
//
// The longest measured is 89.2 s, on a sandbox that was ~86 s old when it was
// paused; the stall tracks that age, so this number is a floor for sandboxes of
// that age and not a ceiling for older ones. Raising the constant is not how an
// older one gets covered -- see credpush.go.
func TestCredPushDeadlineOutlastsTheMeasuredGuestStall(t *testing.T) {
	const measuredStall = 89200 * time.Millisecond
	if credPushDeadline <= measuredStall {
		t.Fatalf("credPushDeadline is %v, which is not longer than the %v stall measured on the "+
			"reference host; see fix-guest-thaw-clock-skew/tasks.md", credPushDeadline, measuredStall)
	}
	// daemon.wakeTimeout, which this package cannot import. An auto-wake's caller
	// gives up there while the restore it started keeps running, so a push
	// deadline under it fails restores the caller has not yet abandoned.
	const daemonWakeTimeout = 90 * time.Second
	if credPushDeadline <= daemonWakeTimeout {
		t.Fatalf("credPushDeadline %v must outlast daemon.wakeTimeout (%v)", credPushDeadline, daemonWakeTimeout)
	}
	// Each attempt is bounded by credPushTimeout, so the loop must fit several.
	if credPushDeadline <= credPushTimeout {
		t.Fatalf("credPushDeadline %v must exceed one attempt's %v", credPushDeadline, credPushTimeout)
	}
}

// pushCredential performs one push and reports whether envd accepted it. Only
// the tests push a bare credential; production goes through deliverCredential.
func pushCredential(udsPath string, port int, tokenHash string) error {
	return pushCommand(udsPath, port, credSetHashCommand+" "+tokenHash)
}
