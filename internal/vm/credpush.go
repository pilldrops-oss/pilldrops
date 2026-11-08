//go:build linux

package vm

// Host end of envd's credential channel. A golden snapshot's restored memory
// holds no per-sandbox token hash, so the hash is pushed after resume over the
// VM's own jailer-chroot vsock UDS, which no guest can reach.
//
// Wire format, fixed by cmd/envd/credential.go:
//
//	-> "CONNECT 6002\n"        (Firecracker's host-side vsock handshake)
//	<- "OK <hostport>\n"       (the guest kernel completed the connection)
//	-> "SETHASH <64-hex>\n"
//	<- "OK\n" | "ERR <why>\n"  (written by envd)
//
// Only the second reply means the push landed; Firecracker's ack says nothing
// about whether any guest process accepted.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"time"
)

const (
	// credVsockPort must equal cmd/envd/main.go's credVsockPort.
	credVsockPort = 6002

	// credPushTimeout bounds one attempt; the peer is a local UNIX socket, so
	// slow means dead and the caller retries.
	credPushTimeout = 3 * time.Second

	// credPushDeadline bounds the retry loop. A just-resumed VM may not have its
	// UDS or listener back yet -- and, measured on the reference host, its envd
	// may not be *scheduled* yet: a guest restored from a memory image
	// intermittently stops executing anything a timer drives -- a 1 Hz timer in
	// the guest managed one tick in 303 s while its clock ran normally and it
	// still answered TCP -- and then recovers on its own. 15 s was shorter than
	// that stall, which is the whole of the intermittent "credential not accepted by
	// the resumed guest" resume failure. add-sandbox-standby/tasks.md has the
	// goroutine dump.
	//
	// The stall's length is the wall time the VM had been alive when it was
	// snapshotted -- not how long it spent frozen on standby, which is what that
	// note inferred and what fix-guest-thaw-clock-skew measured and disproved:
	// three of eight restores of ~86-second-old sandboxes stalled, all three at
	// 88.7-89.2 s, in equal numbers whether the sandbox had spent 59 s of its
	// life frozen or had never been frozen at all. The slope is 1:1 and has no
	// ceiling -- a never-frozen 300-second-old sandbox stalled 303.9 s.
	//
	// This is the deadline for a guest that is not answering at all, and it
	// stays: an auto-wake's caller is bounded by daemon.wakeTimeout (90 s), so
	// anything shorter fails restores the caller is still waiting for, and
	// anything longer only makes a truly absent guest take longer to declare.
	// A stalled guest gets credPushStalledDeadline instead -- see below.
	credPushDeadline = 120 * time.Second

	// credPushStalledDeadline applies once the guest kernel has accepted a
	// connection on the credential port and nothing inside has answered it,
	// which is the stall and not a dead guest: Firecracker only acks CONNECT
	// when the guest kernel finds a listener with room in its accept queue.
	//
	// It has to be much larger than credPushDeadline because the stall is as
	// long as the VM had been alive, and 120 s does not merely make such a
	// resume slow -- it makes it *impossible*. Measured: a five-minute-old
	// sandbox stalled 303.9 s, so every one of its restores failed at 120 s, and
	// the snapshot's stall is the same length every time, so retrying it failed
	// identically three times in a row. That sandbox is unrecoverable on a fixed
	// 120 s; with this it comes back in ~304 s, after the caller has given up
	// but well before anyone would notice a lost sandbox, because the restore
	// runs on without the caller (see ensureRunning's context.WithoutCancel).
	//
	// 15 minutes is a choice, and it is the one thing here not pinned to a
	// measurement: it covers a sandbox up to about a quarter of an hour old and
	// bounds how long a restored VM may hold its memory waiting. Nothing covers
	// an older one but fixing the stall, which is what makes it a ceiling worth
	// having rather than a number worth growing. Not reproduced on Firecracker
	// v1.17.0 with host kernel 7.0 (0 of 22, see docs/deployment.md), so it is
	// kept as a safety net that logs when it fires.
	credPushStalledDeadline = 15 * time.Minute

	credPushRetryInterval = 100 * time.Millisecond
)

// ErrCredentialPush wraps every credential-delivery failure so a create error names the push.
var ErrCredentialPush = errors.New("could not deliver the sandbox credential to the resumed guest")

// ErrVsockUDSNotFound means the VM has no host-side vsock socket to dial; a
// node path problem, not a guest refusal.
var ErrVsockUDSNotFound = errors.New("no vsock UDS for this VM's jailer chroot")

// ErrGuestNoReply means the guest kernel accepted the connection and nothing in
// the guest answered it. It is the one failure that proves the guest is alive,
// because Firecracker acks CONNECT only when the guest kernel finds a listener
// with room in its accept queue -- so it is what separates "stalled" from
// "gone", and the only one worth waiting out.
var ErrGuestNoReply = errors.New("the guest accepted the connection but nothing in it replied")

// vsockUDSPath discovers the host path of a VM's vsock UDS rather than
// rebuilding it: the chroot is named for the jailer id, not the sandbox id, and
// on restore the socket file keeps the snapshotted device's name.
func vsockUDSPath(chrootRoot, sandboxID string) (string, error) {
	root := chrootRoot
	if root == "" {
		// Fallback for machines that cannot report a chroot (no jailer).
		root = filepath.Join(jailerChrootDir(), sandboxID, "root")
	}
	runDir := filepath.Join(root, "run")

	matches, err := filepath.Glob(filepath.Join(runDir, "*.vsock"))
	if err != nil {
		return "", fmt.Errorf("%w: globbing %s: %w", ErrVsockUDSNotFound, runDir, err)
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("%w: %s contains no *.vsock socket", ErrVsockUDSNotFound, runDir)
	}

	// Never guess between sockets: the wrong one belongs to another VM's guest.
	want := filepath.Join(runDir, sandboxID+".vsock")
	for _, m := range matches {
		if m == want {
			return m, nil
		}
	}
	return "", fmt.Errorf("%w: %s contains %d *.vsock sockets and none is named for %s: %v",
		ErrVsockUDSNotFound, runDir, len(matches), sandboxID, matches)
}

// pushCommand does one request/reply exchange over the VM's vsock UDS. Both
// replies are read and only the second counts: Firecracker acks CONNECT as
// soon as the guest kernel connects, even with no listener behind it.
func pushCommand(udsPath string, port int, command string) error {
	verb := command
	if i := strings.IndexByte(verb, ' '); i > 0 {
		verb = verb[:i]
	}

	c, err := (&net.Dialer{Timeout: credPushTimeout}).DialContext(context.Background(), "unix", udsPath)
	if err != nil {
		return fmt.Errorf("%w: dial %s: %w", ErrCredentialPush, udsPath, err)
	}
	defer func() { _ = c.Close() }()
	if err := c.SetDeadline(time.Now().Add(credPushTimeout)); err != nil {
		return fmt.Errorf("%w: set deadline on %s: %w", ErrCredentialPush, udsPath, err)
	}
	br := bufio.NewReader(c)

	// Firecracker's handshake; its ack only means the guest kernel connected.
	if _, err := fmt.Fprintf(c, "CONNECT %d\n", port); err != nil {
		return fmt.Errorf("%w: write CONNECT to %s: %w", ErrCredentialPush, udsPath, err)
	}
	ack, err := br.ReadString('\n')
	if err != nil {
		return fmt.Errorf("%w: no CONNECT ack from %s (guest vsock port %d): %w",
			ErrCredentialPush, udsPath, port, err)
	}
	if !strings.HasPrefix(ack, "OK") {
		return fmt.Errorf("%w: firecracker refused CONNECT %d on %s: %q",
			ErrCredentialPush, port, udsPath, strings.TrimSpace(ack))
	}

	// envd writes this reply itself after applying the command.
	if _, err := fmt.Fprintf(c, "%s\n", command); err != nil {
		return fmt.Errorf("%w: write %s to %s: %w", ErrCredentialPush, verb, udsPath, err)
	}
	reply, err := br.ReadString('\n')
	if err != nil {
		return fmt.Errorf("%w: %w: connected to guest vsock port %d on %s but envd sent no reply "+
			"(the vsock device works; nothing in the guest accepted): %v",
			ErrCredentialPush, ErrGuestNoReply, port, udsPath, err)
	}
	if got := strings.TrimSpace(reply); got != "OK" {
		return fmt.Errorf("%w: envd refused %s on %s: %q", ErrCredentialPush, verb, udsPath, got)
	}
	return nil
}

// credSetHashCommand mirrors cmd/envd/credential.go's credSetHash.
const credSetHashCommand = "SETHASH"

// credSetNetCommand mirrors cmd/envd/netconfig.go's credSetNet.
const credSetNetCommand = "SETNET"

// deliverCredential pushes tokenHash to the guest, retrying until deadline
// because the UDS and envd's listener may not be back the instant the VM
// resumes. Returns the last error, since the first is usually just the race.
func deliverCredential(chrootRoot, sandboxID, tokenHash string, deadline time.Duration) error {
	if tokenHash == "" {
		// Name the problem here rather than relay envd's rejection of an empty hash.
		return fmt.Errorf("%w: no token hash to deliver for %s", ErrCredentialPush, sandboxID)
	}
	// The credential is the one push worth waiting a stall out for: without it
	// the restore fails and the sandbox stays paused, with the same stall
	// waiting for every retry.
	if err := pushToGuestWaitingOutStalls(chrootRoot, sandboxID,
		credSetHashCommand+" "+tokenHash, deadline, credPushStalledDeadline); err != nil {
		return fmt.Errorf("credential %w", err)
	}
	return nil
}
