package orchestrator

// The public end of a port forward. A raw TCP port has to terminate on one
// host with a public address, so exactly one replica runs this — the one
// started with FORWARD_INGRESS=1 — and every other replica serves the API
// only. The row is the truth; this reconciles listeners against it.

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/httpx"
	"github.com/pilldrops-oss/pilldrops/internal/nodetls"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// forwardReconcileEvery is the safety net behind the add/delete triggers: an
// add on another replica reaches this one only on the next tick.
const forwardReconcileEvery = 30 * time.Second

// forwardDialTimeout bounds the TCP connect and the pinned TLS handshake.
const forwardDialTimeout = 10 * time.Second

// forwardWakeTimeout bounds the 101 handshake, which is a different thing
// entirely: the node holds the request open while it RESUMES a paused sandbox
// and waits for the app to bind. Measured at 18 s for a docker restore, so a
// dial-sized deadline here silently resets the first connection after every
// wake and looks like a broken forward.
const forwardWakeTimeout = 90 * time.Second

// forwardIngress owns the bound public ports on this replica.
type forwardIngress struct {
	s  *Server
	mu sync.Mutex
	// tcp is bound public port -> listener. A port present here is being served.
	tcp map[int]*forwardListener
	// udp is the same for the udp half; a row may carry either or both.
	udp map[int]*udpForward
}

// forwardListener is one public TCP port plus the connections open on it, so
// removing the row can close them rather than leaving a stream pointed at a
// destroyed sandbox.
type forwardListener struct {
	ln net.Listener
	// org owns the sandbox, resolved once at bind: the source gate is per org
	// and a sandbox never changes hands, so a store read per connection would
	// buy nothing.
	org   string
	row   store.PortForward
	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

// currentRow reads the row this listener is serving. Guarded by the listener's
// own mutex, not the ingress map's: serve() reads it on every connection while
// reconcile may be updating it.
func (fl *forwardListener) currentRow() store.PortForward {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	return fl.row
}

func (fl *forwardListener) setRow(row store.PortForward) {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	fl.row = row
}

func (fl *forwardListener) track(c net.Conn) {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	if fl.conns == nil {
		fl.conns = map[net.Conn]struct{}{}
	}
	fl.conns[c] = struct{}{}
}

func (fl *forwardListener) untrack(c net.Conn) {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	delete(fl.conns, c)
}

// close stops accepting and tears down every open stream on this port.
func (fl *forwardListener) close() {
	_ = fl.ln.Close() //nolint:errcheck // best-effort
	fl.mu.Lock()
	defer fl.mu.Unlock()
	for c := range fl.conns {
		_ = c.Close() //nolint:errcheck // best-effort: the splice goroutines see EOF
	}
	fl.conns = nil
}

// ForwardIngressEnabled reports whether this replica should bind public ports.
func ForwardIngressEnabled() bool {
	v := os.Getenv("FORWARD_INGRESS")
	return v == "1" || v == "true" || v == "yes"
}

// forwardIngressDeclared reports whether ANY replica of this deployment binds
// public forward ports: this one, or another that the operator declared by
// setting FORWARD_INGRESS=remote here. Unset is the default, and it means
// nothing will ever bind, so the API refuses the create instead of answering
// 201 with an address that refuses connections.
//
// An operator's word, not a probe: only the replica holding the socket knows,
// and it is not this one. That is what the `ingress_id` column in
// docs/deployment.md's upgrade path would turn into a fact.
func forwardIngressDeclared() bool {
	if ForwardIngressEnabled() {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(os.Getenv("FORWARD_INGRESS")), "remote")
}

// StartForwardIngress binds this replica's public forward ports and keeps them
// reconciled until ctx is cancelled. Safe to call on a replica that is not the
// ingress: it returns immediately.
func (s *Server) StartForwardIngress(ctx context.Context) {
	if !ForwardIngressEnabled() {
		return
	}
	fi := &forwardIngress{s: s, tcp: map[int]*forwardListener{}, udp: map[int]*udpForward{}}
	s.ingress.Store(fi)
	lo, hi := forwardRange()
	s.log().Info("port-forward ingress live", "component", "forward",
		"range", strconv.Itoa(lo)+"-"+strconv.Itoa(hi), "public_host", s.forwardPublicHost())
	go func() {
		t := time.NewTicker(forwardReconcileEvery)
		defer t.Stop()
		for {
			s.reconcileForwards(ctx)
			select {
			case <-ctx.Done():
				fi.closeAll()
				return
			case <-t.C:
			}
		}
	}()
}

func (fi *forwardIngress) closeAll() {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	for p, fl := range fi.tcp {
		fl.close()
		delete(fi.tcp, p)
	}
	for p, u := range fi.udp {
		u.close()
		delete(fi.udp, p)
	}
}

// reconcileForwards makes the bound ports match the active rows. A no-op on a
// replica that is not the ingress, which is what lets the API run anywhere.
// Holds fi.mu across the diff; reconcileUDP assumes that.
func (s *Server) reconcileForwards(ctx context.Context) {
	fi := s.ingress.Load()
	if fi == nil {
		return
	}
	st := s.st.Load()
	if st == nil {
		return
	}
	rows, err := st.ActivePortForwards(ctx)
	if err != nil {
		// Keep what is bound: a database blip must not drop live listeners.
		s.log().ErrorContext(ctx, "port-forward reconcile could not read the rows; keeping the bound ports",
			"component", "forward", "err", err)
		return
	}
	want, wantUDP := map[int]store.PortForward{}, map[int]store.PortForward{}
	for _, f := range rows {
		if f.TCP {
			want[f.PublicPort] = f
		}
		if f.UDP {
			wantUDP[f.PublicPort] = f
		}
	}

	fi.mu.Lock()
	defer fi.mu.Unlock()
	fi.reconcileUDP(ctx, wantUDP)
	for port, fl := range fi.tcp {
		had := fl.currentRow()
		row, ok := want[port]
		if ok && row.SandboxID == had.SandboxID && row.GuestPort == had.GuestPort {
			fl.setRow(row)
			continue
		}
		// Gone, or the port was reallocated to a different sandbox: close it and
		// every stream on it, so an open connection to a destroyed sandbox ends.
		fl.close()
		delete(fi.tcp, port)
		s.log().InfoContext(ctx, "port forward closed", "component", "forward",
			"sandbox", had.SandboxID, "public_port", port, "guest_port", had.GuestPort)
	}
	for port, row := range want {
		if _, bound := fi.tcp[port]; bound {
			continue
		}
		ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", ":"+strconv.Itoa(port))
		if err != nil {
			s.log().ErrorContext(ctx, "port forward could not bind its public port",
				"component", "forward", "sandbox", row.SandboxID, "public_port", port, "err", err)
			continue
		}
		org, _ := s.previewOrgOf(ctx, row.SandboxID)
		fl := &forwardListener{ln: ln, org: org, row: row}
		fi.tcp[port] = fl
		s.log().InfoContext(ctx, "port forward bound", "component", "forward",
			"sandbox", row.SandboxID, "public_port", port, "guest_port", row.GuestPort)
		go fi.accept(fl)
	}
}

func (fi *forwardIngress) accept(fl *forwardListener) {
	for {
		c, err := fl.ln.Accept()
		if err != nil {
			return // the listener was closed by reconcile or shutdown
		}
		go fi.serve(fl, c)
	}
}

// serve relays one accepted connection to the sandbox's node. A sandbox that
// has gone, or a node that will not answer, closes the connection immediately
// rather than leaving the client hanging on a port that cannot deliver.
func (fi *forwardIngress) serve(fl *forwardListener, client net.Conn) {
	fl.track(client)
	defer fl.untrack(client)

	row := fl.currentRow()

	// The org's source-network restriction, on the path it never covered. A raw
	// forwarded port carries no X-Forwarded-For to argue about, so this is the
	// true peer — a better answer than the HTTP gate gets. Closed without a
	// byte in reply, which is what an unreachable port looks like.
	if host, _, err := net.SplitHostPort(client.RemoteAddr().String()); err != nil ||
		!fi.s.peerIPAllowed(context.Background(), fl.org, host) {
		_ = client.Close() //nolint:errcheck // best-effort
		return
	}

	node, err := fi.s.dialNodeTunnel(context.Background(), row.SandboxID, row.GuestPort, "tcp")
	if err != nil {
		fi.s.log().Warn("port forward could not reach the sandbox; closing the connection",
			"component", "forward", "sandbox", row.SandboxID, "guest_port", row.GuestPort,
			"public_port", row.PublicPort, "err", err)
		_ = client.Close() //nolint:errcheck // best-effort
		return
	}
	httpx.Splice(client, node)
}

// dialNodeTunnel opens a raw duplex stream to a guest port through the node's
// /tcp/ (or /udp/) route. It cannot use s.client: an http.Client never hands
// back the socket, and this needs the socket. So the dial, the pinned TLS
// handshake and the request are written by hand, and the response is parsed
// with http.ReadResponse so the 101 is still a real HTTP answer.
func (s *Server) dialNodeTunnel(ctx context.Context, id string, port int, proto string) (net.Conn, error) {
	node, ok := s.reg.NodeFor(id)
	if !ok {
		return nil, fmt.Errorf("sandbox %s is not placed on any node", id)
	}
	// nodeHop is reused for its two policy decisions only: refuse an unpinned
	// hop when SANDBOXD_REQUIRE_SECURE_HOP is set, and upgrade a pinned node's
	// http:// URL to https://. The client it returns is discarded.
	base, _, err := s.nodeHop(node, hopStream)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("node %s has an unparseable URL %q: %w", node.ID, base, err)
	}
	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "https" {
			host = net.JoinHostPort(u.Hostname(), "443")
		} else {
			host = net.JoinHostPort(u.Hostname(), "80")
		}
	}

	// The gate on the data path. A public forward port has no request to
	// authenticate and no token to ask for, so the row carries the permission
	// and this asks the same ownership question the create path asked — for
	// the only principal a raw connection speaks for: nobody. Without it an
	// anonymous client was spliced into a private sandbox the preview path
	// refuses, and the two paths shared the predicate in name only.
	if st := s.st.Load(); st != nil {
		row, found, err := st.GetActiveSandboxRow(ctx, id)
		if err != nil || !found {
			return nil, fmt.Errorf("sandbox %s: no row to judge who may reach it, so the forward is refused", id)
		}
		if !sandbox.ReachableAnonymously(row.Visibility, row.CreatedBy) {
			return nil, fmt.Errorf("sandbox %s is private: a forward publishes a port with no credential on it, "+
				"so it serves only an org-visible sandbox", id)
		}
	}

	tok := s.nodePreviewToken(ctx, id, port)
	if tok == "" {
		return nil, fmt.Errorf("no node token configured, so no forward credential can be minted")
	}

	dialCtx, cancel := context.WithTimeout(ctx, forwardDialTimeout)
	defer cancel()
	conn, err := httpx.DialNode(dialCtx, u, host)
	if err != nil {
		return nil, err
	}
	if u.Scheme == "https" {
		var cfg *tls.Config
		if node.TLSSPKI != "" {
			if cfg, err = nodetls.PinnedClientConfig(node.TLSSPKI); err != nil {
				_ = conn.Close() //nolint:errcheck // best-effort
				return nil, err
			}
		} else {
			cfg = &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}
		}
		tc := tls.Client(conn, cfg)
		if err := tc.HandshakeContext(dialCtx); err != nil {
			_ = conn.Close() //nolint:errcheck // best-effort
			return nil, fmt.Errorf("node %s tls handshake: %w", node.ID, err)
		}
		conn = tc
	}
	// The wake budget, not the dial budget: the node answers only once the
	// sandbox is running and its app has bound.
	_ = conn.SetDeadline(time.Now().Add(forwardWakeTimeout)) //nolint:errcheck // cleared below once the stream is up

	req := &http.Request{
		Method: http.MethodGet,
		URL: &url.URL{
			Path:     "/v1/sandboxes/" + id + "/" + proto + "/" + strconv.Itoa(port),
			RawQuery: "ptok=" + url.QueryEscape(tok),
		},
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Host:   u.Host,
		Header: http.Header{"Connection": {"Upgrade"}, "Upgrade": {"pilldrops-l4"}},
	}
	if err := req.Write(conn); err != nil {
		_ = conn.Close() //nolint:errcheck // best-effort
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		_ = conn.Close() //nolint:errcheck // best-effort
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = resp.Body.Close() //nolint:errcheck // best-effort
		_ = conn.Close()      //nolint:errcheck // best-effort
		return nil, fmt.Errorf("node %s refused the forward for sandbox %s port %d: %s",
			node.ID, id, port, resp.Status)
	}
	// The stream is open; the handshake deadline must not kill it.
	_ = conn.SetDeadline(time.Time{}) //nolint:errcheck // best-effort
	return httpx.Prefixed(conn, br), nil
}
