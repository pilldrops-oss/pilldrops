package orchestrator

// The public end of a UDP forward. Same row, same public port, second
// listener: one long-lived stream to the node carries every client of the
// port, framed so datagram boundaries and the sender survive the hop.
//
// ponytail: one stream per public port, so a burst from one client can
// head-of-line-block another. Upgrade path is one stream per client address if
// a customer's UDP workload ever needs it.

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/httpx"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// udpForward is one bound public UDP port and the stream currently carrying it.
type udpForward struct {
	pc  *net.UDPConn
	org string // owner, resolved once at bind; see forwardListener.org
	row store.PortForward

	mu     sync.Mutex
	stream net.Conn // nil until the first datagram, and after a break
	// cidrs is the org's source restriction, refreshed at most every
	// sourceCacheTTL. Held here because this gate is consulted per datagram and
	// a store read per packet is a denial of service with extra steps.
	cidrs    []netip.Prefix
	cidrsExp time.Time
}

// udpPeerAllowed applies the org's source-network restriction to one datagram's
// sender. Per datagram, not per stream: one stream carries every client of this
// port, so checking only when it opens would let a peer outside the org's
// networks ride a stream a permitted client opened.
//
// ponytail: a restriction set while a port is bound takes up to sourceCacheTTL
// to reach this listener, unlike every other caller of the gate. Drop the TTL
// or invalidate from the PUT handler if that window ever matters.
func (fi *forwardIngress) udpPeerAllowed(u *udpForward, from netip.Addr) bool {
	u.mu.Lock()
	cidrs, exp := u.cidrs, u.cidrsExp
	u.mu.Unlock()
	if !fi.s.now().Before(exp) {
		cidrs = fi.s.orgSourceCIDRs(context.Background(), u.org)
		u.mu.Lock()
		u.cidrs, u.cidrsExp = cidrs, fi.s.now().Add(sourceCacheTTL)
		u.mu.Unlock()
	}
	return peerInCIDRs(from.String(), cidrs)
}

// close stops the listener and tears down the stream.
func (u *udpForward) close() {
	_ = u.pc.Close() //nolint:errcheck // best-effort
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.stream != nil {
		_ = u.stream.Close() //nolint:errcheck // best-effort
		u.stream = nil
	}
}

// reconcileUDP is reconcileForwards for the udp half. Kept separate because the
// two protocols share a row but not a socket, and a forward may carry either.
func (fi *forwardIngress) reconcileUDP(ctx context.Context, want map[int]store.PortForward) {
	for port, u := range fi.udp {
		row, ok := want[port]
		if ok && row.SandboxID == u.row.SandboxID && row.GuestPort == u.row.GuestPort {
			continue
		}
		u.close()
		delete(fi.udp, port)
		fi.s.log().InfoContext(ctx, "udp forward closed", "component", "forward",
			"sandbox", u.row.SandboxID, "public_port", port, "guest_port", u.row.GuestPort)
	}
	for port, row := range want {
		if _, bound := fi.udp[port]; bound {
			continue
		}
		pc, err := net.ListenUDP("udp", &net.UDPAddr{Port: port})
		if err != nil {
			fi.s.log().ErrorContext(ctx, "udp forward could not bind its public port",
				"component", "forward", "sandbox", row.SandboxID, "public_port", port, "err", err)
			continue
		}
		org, _ := fi.s.previewOrgOf(ctx, row.SandboxID)
		u := &udpForward{pc: pc, org: org, row: row}
		fi.udp[port] = u
		fi.s.log().InfoContext(ctx, "udp forward bound", "component", "forward",
			"sandbox", row.SandboxID, "public_port", port, "guest_port", row.GuestPort)
		go fi.pumpUDP(u)
	}
}

// pumpUDP forwards every datagram arriving on the public port, opening the
// stream to the node on the first one.
func (fi *forwardIngress) pumpUDP(u *udpForward) {
	buf := make([]byte, httpx.MaxDatagram)
	for {
		n, from, err := u.pc.ReadFromUDPAddrPort(buf)
		if err != nil {
			return // the listener was closed
		}
		// Dropped silently, like any other datagram that does not arrive.
		if !fi.udpPeerAllowed(u, from.Addr().Unmap()) {
			continue
		}
		s, err := fi.udpStream(u)
		if err != nil {
			fi.s.log().Warn("udp forward could not reach the sandbox; dropping the datagram",
				"component", "forward", "sandbox", u.row.SandboxID,
				"public_port", u.row.PublicPort, "err", err)
			continue
		}
		u.mu.Lock()
		wErr := httpx.WriteDatagram(s, netip.AddrPortFrom(from.Addr().Unmap(), from.Port()), buf[:n])
		if wErr != nil {
			// A broken stream is reopened on the next datagram, not retried here:
			// UDP callers already tolerate a loss, and a retry loop would stall the
			// reader for every other client on this port.
			_ = s.Close() //nolint:errcheck // best-effort
			u.stream = nil
		}
		u.mu.Unlock()
	}
}

// udpStream returns the live stream to the node, opening one if needed.
func (fi *forwardIngress) udpStream(u *udpForward) (net.Conn, error) {
	u.mu.Lock()
	if s := u.stream; s != nil {
		u.mu.Unlock()
		return s, nil
	}
	u.mu.Unlock()

	// Dialled outside the lock: it can take seconds when the sandbox has to wake.
	ctx, cancel := context.WithTimeout(context.Background(), forwardWakeTimeout)
	defer cancel()
	s, err := fi.s.dialNodeTunnel(ctx, u.row.SandboxID, u.row.GuestPort, "udp")
	if err != nil {
		return nil, err
	}

	u.mu.Lock()
	if u.stream != nil { // another datagram won the race
		u.mu.Unlock()
		_ = s.Close() //nolint:errcheck // best-effort
		u.mu.Lock()
		defer u.mu.Unlock()
		return u.stream, nil
	}
	u.stream = s
	u.mu.Unlock()
	go fi.pumpUDPReplies(u, s)
	return s, nil
}

// pumpUDPReplies sends each framed reply back to the client address it carries.
func (fi *forwardIngress) pumpUDPReplies(u *udpForward, s net.Conn) {
	for {
		addr, payload, err := httpx.ReadDatagram(s)
		if err != nil {
			u.mu.Lock()
			if u.stream == s {
				u.stream = nil
			}
			u.mu.Unlock()
			_ = s.Close() //nolint:errcheck // best-effort
			return
		}
		if !addr.IsValid() {
			continue
		}
		_ = u.pc.SetWriteDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck // best-effort
		_, _ = u.pc.WriteToUDPAddrPort(payload, addr)              //nolint:errcheck // a dropped reply is UDP working as designed
	}
}
