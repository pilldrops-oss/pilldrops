package main

// This sandbox's credential and the channel a node uses to deliver one after
// boot, which a snapshot-restored guest needs because its memory holds the
// golden VM's boot arguments. The protocol and store are platform-independent
// so the security property is tested everywhere; only vsock.go is Linux-only.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// credSetHash sets this sandbox's credential: `SETHASH <64-hex>`.
	credSetHash = "SETHASH"
	// credSetTime steps the guest's wall clock: `SETTIME <unix_nanoseconds>`.
	// A guest restored from a memory image believes the time is whatever it was
	// when that image was written, and has no other way to find out.
	credSetTime = "SETTIME"
	// credMaxLine bounds a request; no valid command is longer (SETIDENTITY
	// carries a 70-byte token, a URL, an id and a domain).
	credMaxLine = 512
	// credConnTimeout stops a node that died mid-push from parking a goroutine forever.
	credConnTimeout = 5 * time.Second
)

// hashStore holds this sandbox's envd token hash; readers call Get at call
// time so a credential delivered after boot is visible immediately.
// path, when set, keeps a pushed hash on disk for a restarted server.
type hashStore struct {
	v    atomic.Value
	path string
}

func newHashStore(hash string) *hashStore {
	s := &hashStore{}
	s.v.Store(hash)
	return s
}

// pushedHashPath is where the guest server keeps the last pushed hash. A
// restored guest gets its credential once, over vsock, so a server the
// reaper restarts would otherwise refuse every call.
const pushedHashPath = "/run/envd.token-hash"

// persistedHashStore starts from the last pushed hash at path, which outranks
// boot because a capture restored as another sandbox still carries the
// capturing one's cmdline. Docker's env hash outranks both: it never pushes.
func persistedHashStore(path string) *hashStore {
	hash := strings.TrimSpace(os.Getenv("ENVD_TOKEN_HASH"))
	if hash == "" {
		if b, err := os.ReadFile(path); err == nil && validTokenHash(strings.TrimSpace(string(b))) {
			hash = strings.TrimSpace(string(b))
		}
	}
	if hash == "" {
		hash = readTokenHash()
	}
	s := newHashStore(hash)
	s.path = path
	return s
}

// Get returns the current hash, or "" when none has been provisioned.
func (s *hashStore) Get() string {
	h, _ := s.v.Load().(string)
	return h
}

func (s *hashStore) Set(hash string) {
	s.v.Store(hash)
	if s.path == "" {
		return
	}
	if err := os.WriteFile(s.path, []byte(hash), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "envd: keep pushed credential: %v\n", err)
	}
}

// validTokenHash reports whether h is 64 lower-case hex characters, so a
// mangled push cannot install a hash the node believes is live.
func validTokenHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// handleCredCommand applies one request line and returns the reply line.
// The reply is a payload the node reads: Firecracker acks a CONNECT as soon as
// the guest kernel completes it, which proves nothing about this process.
func handleCredCommand(line string, hashes *hashStore) string {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) == 0 {
		return "ERR empty command"
	}
	switch fields[0] {
	case credSetHash:
		if len(fields) != 2 {
			return "ERR " + credSetHash + " takes exactly one argument"
		}
		if !validTokenHash(fields[1]) {
			return "ERR argument is not a 64-character lower-case hex sha-256"
		}
		hashes.Set(fields[1])
		return "OK"

	case credSetTime:
		if len(fields) != 2 {
			return "ERR " + credSetTime + " takes exactly one argument"
		}
		ns, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || ns <= 0 {
			return "ERR argument is not a positive unix nanosecond timestamp"
		}
		if err := setGuestClock(ns); err != nil {
			return "ERR could not set the guest clock: " + err.Error()
		}
		return "OK"

	case credSetNet:
		// Applied before replying: the node bridges the tap on OK, and a guest
		// still holding the golden identity must not reach the shared bridge.
		c, err := parseNetConfig(fields[1:])
		if err != nil {
			return "ERR " + err.Error()
		}
		if err := applyNetConfig(c); err != nil {
			return "ERR could not apply " + c.String() + ": " + err.Error()
		}
		return "OK"

	case credSetHost:
		if len(fields) != 2 {
			return "ERR " + credSetHost + " takes exactly one argument"
		}
		if err := applyHostname(fields[1]); err != nil {
			return "ERR could not set the hostname: " + err.Error()
		}
		return "OK"

	case credSetIdentity:
		// OK only once both files are on disk: the node hands the sandbox out
		// on this reply, and a guest without its token cannot act as itself.
		id, err := parseIdentity(fields[1:])
		if err != nil {
			return "ERR " + err.Error()
		}
		if err := applyIdentity(identityDir, id); err != nil {
			return "ERR could not write " + identityDir + ": " + err.Error()
		}
		return "OK"
	}
	// Not echoed: attacker-influenced input, and the node may log the reply.
	return "ERR unknown command"
}

// credPeer is implemented by the AF_VSOCK addresses vsock.go produces.
type credPeer interface{ ContextID() uint32 }

// credPeerAllowed reports whether a connection may push a credential. Only the
// host may: the listener binds VMADDR_CID_ANY, which the guest's own processes
// can also reach over loopback, and a tenant must not set its own hash.
func credPeerAllowed(a net.Addr) (bool, string) {
	p, ok := a.(credPeer)
	if !ok {
		return false, fmt.Sprintf("peer %v is not an AF_VSOCK address", a)
	}
	if cid := p.ContextID(); cid != unix_VMADDR_CID_HOST {
		return false, fmt.Sprintf("peer context id %d is not the host (%d)", cid, unix_VMADDR_CID_HOST)
	}
	return true, ""
}

// serveCredConn reads one command from c, applies it and replies; one exchange per connection.
func serveCredConn(c net.Conn, hashes *hashStore) {
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(credConnTimeout))

	if ok, why := credPeerAllowed(c.RemoteAddr()); !ok {
		fmt.Fprintf(os.Stderr, "envd: credential push REFUSED: %s\n", why)
		_, _ = io.WriteString(c, "ERR only the host may push a credential\n")
		return
	}

	br := bufio.NewReader(io.LimitReader(c, credMaxLine))
	line, err := br.ReadString('\n')
	if err != nil && line == "" {
		return
	}
	_, _ = io.WriteString(c, handleCredCommand(line, hashes)+"\n")
}

// serveCredChannel accepts credential pushes until l is closed.
func serveCredChannel(l net.Listener, hashes *hashStore) {
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// A repeating accept error would otherwise burn a core silently.
			fmt.Fprintf(os.Stderr, "envd: credential channel accept: %v\n", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go serveCredConn(c, hashes)
	}
}

// credChannelAllowed reports whether this process may open a credential channel.
// Trusted only because the host end is a UNIX socket in this VM's jailer chroot,
// so refused for containers (shared vsock namespace) and non-guest context ids.
func credChannelAllowed(envHash string, localCID uint32) (bool, string) {
	if strings.TrimSpace(envHash) != "" {
		return false, "ENVD_TOKEN_HASH is set, so a container runtime provisioned this envd; " +
			"containers share the node's vsock namespace and a sibling could overwrite the credential"
	}
	if localCID <= unix_VMADDR_CID_HOST {
		return false, fmt.Sprintf("local vsock context id is %d, which is a hypervisor/local/host id, "+
			"not a guest (>= %d); the per-VM jailer chroot this channel's trust rests on does not exist here",
			localCID, unix_VMADDR_CID_HOST+1)
	}
	return true, ""
}

// unix_VMADDR_CID_HOST mirrors unix.VMADDR_CID_HOST, which x/sys/unix does not define on every platform.
const unix_VMADDR_CID_HOST = 2

// onceLogger prints a message exactly once, however many times it is asked.
type onceLogger struct{ once sync.Once }

func (o *onceLogger) log(w io.Writer, msg string) {
	o.once.Do(func() { _, _ = io.WriteString(w, msg) })
}

// transportNotice describes the bind, its reason, and the credential channel's state or error.
func transportNotice(network, reason string, credPort int, credErr error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "envd: control plane on %s — %s\n", network, reason)
	if credErr != nil {
		fmt.Fprintf(&b, "envd: vsock credential channel UNAVAILABLE on port %d: %v\n", credPort, credErr)
		fmt.Fprintf(&b, "envd: credentials are limited to what boot provided "+
			"(ENVD_TOKEN_HASH / envd.token_hash=); a snapshot-restored sandbox cannot be given one\n")
		return b.String()
	}
	fmt.Fprintf(&b, "envd: vsock credential channel listening on port %d\n", credPort)
	return b.String()
}
