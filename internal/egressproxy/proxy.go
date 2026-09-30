// Package egressproxy is the name layer: the one component that reads a
// sandbox's connection off the wire so a domain rule can be decided. It reads
// the TLS SNI or the HTTP Host, asks internal/egress for the verdict, and
// splices the bytes through untouched.
//
// It does NOT terminate TLS. The ClientHello is read and replayed byte for
// byte; no platform certificate is ever presented and no tenant plaintext is
// ever available to this process. A sandbox platform that man-in-the-middles
// its tenants is a different product.
//
// The split from internal/egress is deliberate and load-bearing: the decision
// stays pure there so every front end decides identically, and only this
// package touches a socket.
package egressproxy

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"syscall"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/egress"
)

// dialTimeout bounds one upstream connect. A guest waiting on an unreachable
// allow-listed host should get its own timeout, not a goroutine parked here.
const dialTimeout = 10 * time.Second

// Policy is one sandbox's rules plus where to report what was decided. The
// entries are pre-parsed: a parse error must fail the boot, never a connection.
type Policy struct {
	Allow, Deny []egress.Entry
	// SandboxID names the tenant's sandbox in the node log line.
	SandboxID string
	// Self is the proxy's own address inside the namespace. A connection whose
	// original destination is this address was not diverted — the guest dialled
	// the proxy directly — and is refused rather than spliced back to itself.
	Self netip.AddrPort
	// Record is called for every verdict, allowed or denied. Never nil in
	// production; a nil one is tolerated so a test can ignore it.
	Record func(c egress.Conn, v egress.Verdict)
}

func (p Policy) record(c egress.Conn, v egress.Verdict) {
	if p.Record != nil {
		p.Record(c, v)
	}
}

// Serve accepts on l until it is closed, which is how a namespace teardown
// stops this. Each connection is handled on its own goroutine; a slow sniff
// must not hold up the next sandbox connection.
func Serve(l net.Listener, p Policy) {
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		go func() {
			defer func() { _ = c.Close() }()
			handle(c, p)
		}()
	}
}

// handle decides and splices one diverted connection.
func handle(c net.Conn, p Policy) {
	dst, err := originalDst(c)
	if err != nil {
		// Without the pre-NAT destination there is nothing to judge, and
		// guessing would be a hole. Closed, and the guest sees a reset.
		p.record(egress.Conn{}, egress.Verdict{Reason: "could not recover the original destination: " + err.Error()})
		return
	}
	if dst == p.Self {
		p.record(egress.Conn{Addr: dst.Addr(), Port: int(dst.Port())},
			egress.Verdict{Reason: "the name layer's own address is not a destination"})
		return
	}

	port := int(dst.Port())
	name, replay := sniffName(c, port)
	conn := egress.Conn{Name: name, Addr: dst.Addr(), Port: port}
	v := egress.Match(p.Allow, p.Deny, conn)
	p.record(conn, v)
	if !v.Allowed {
		refuse(c, port, v.Reason)
		return
	}

	up, err := dialFor(conn, v)
	if err != nil {
		// A permitted name that will not resolve or connect fails now, closed,
		// rather than as a socket held open on a decision already made.
		refuse(c, port, "allowed, but the destination could not be reached: "+err.Error())
		return
	}
	defer func() { _ = up.Close() }()

	// Everything the sniffer consumed goes upstream first, unchanged: this is
	// the point at which the connection becomes a splice and nothing else.
	if len(replay) > 0 {
		if _, err := up.Write(replay); err != nil {
			return
		}
	}
	splice(c, up)
}

// dialFor opens the upstream connection the verdict permits.
//
// A connection permitted by a DOMAIN entry is dialled BY NAME, resolved here,
// in the same decision that permitted it — not to the address the guest chose.
// That is what closes DNS rebinding: a guest that resolves a permitted name to
// an address of its own and then presents the name is connected to the address
// this resolution returns, and the address it wanted is never dialled.
//
// A connection permitted by an address entry is dialled to that address, which
// is the destination the rule named.
func dialFor(c egress.Conn, v egress.Verdict) (net.Conn, error) {
	// tcp4, not tcp: the policy language is IPv4-only because the guest has no
	// IPv6 stack, so resolving one to a AAAA would dial a destination no rule
	// could ever have been written against.
	d := net.Dialer{Timeout: dialTimeout, Control: refusePrivate}
	if v.Entry.Kind == egress.KindDomain {
		return d.Dial("tcp4", net.JoinHostPort(c.Name, strconv.Itoa(c.Port)))
	}
	return d.Dial("tcp4", netip.AddrPortFrom(c.Addr, uint16(c.Port)).String())
}

// refusePrivate is the last check before connect, on every candidate address
// the resolver returned. This dial leaves the node from outside the sandbox's
// namespace, so the in-namespace drops that keep a guest off the node, the
// other slots and the metadata service are not in its path — an allow-listed
// name that resolves to 169.254.169.254 would otherwise be a way through them.
func refusePrivate(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("egress name layer: unparseable dial address %q", address)
	}
	if addr := ap.Addr().Unmap(); !egress.PublicDest(addr) {
		return fmt.Errorf("egress name layer: %s is not a public destination", addr)
	}
	return nil
}

// refuse tells the guest, in its own protocol where it has one, that policy —
// not the network — closed this. A workload that cannot tell the two apart
// retries a connection that will never succeed.
func refuse(c net.Conn, port int, reason string) {
	if port == egress.PortHTTP {
		body := "egress policy: " + reason + "\n"
		_, _ = io.WriteString(c, "HTTP/1.1 403 Forbidden\r\n"+
			"Content-Type: text/plain; charset=utf-8\r\n"+
			"Content-Length: "+strconv.Itoa(len(body))+"\r\n"+
			"Connection: close\r\n\r\n"+body)
	}
	// On 443 there is nothing to say that is not a certificate, so the
	// handshake is closed. A close is not a hang: the client fails now.
}

// splice copies both directions until both are done, half-closing each side as
// its source ends so a protocol that waits on EOF is not left hanging.
func splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go copyHalf(a, b, done)
	go copyHalf(b, a, done)
	<-done
	<-done
}

func copyHalf(dst, src net.Conn, done chan<- struct{}) {
	_, _ = io.Copy(dst, src)
	if t, ok := dst.(*net.TCPConn); ok {
		_ = t.CloseWrite()
	}
	done <- struct{}{}
}
