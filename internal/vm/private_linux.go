//go:build linux

package vm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"go.uber.org/zap"
	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/sys/unix"

	"github.com/pilldrops-oss/pilldrops/internal/egressproxy"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
)

// liveProxy is one sandbox's private proxy: the listener on its tap gateway,
// the routes it may open, and the sandbox-side connections it holds open.
type liveProxy struct {
	ns string
	// sandboxID is the daemon's id, the one the control plane checks grants
	// against; the map key and teardown use the vm-layer id.
	sandboxID string
	l         net.Listener
	routes    map[string]privateRoute // by IP
	conns     map[net.Conn]*privConn  // by sandbox side
	gate      *privateGate
	// subnets and suffixes are the sandbox's routes; learned holds the addresses
	// split-DNS answers put in the set. Guarded by privMu like the rest.
	subnets  []netip.Prefix
	suffixes []string
	learned  map[netip.Addr]learnedAddr
	dialDest func(ctx context.Context, sandboxID string, dest netip.AddrPort) (net.Conn, error)
	resolve  func(ctx context.Context, sandboxID, name, qtype string) (sandbox.PrivateDNSReply, error)
	// setMu orders writes to the namespace's set, so a refill rendered before
	// an add never lands after it. Taken before privMu, never under it.
	setMu sync.Mutex
}

// privConn is one proxied connection: its target, and its upstream once dialled.
type privConn struct {
	target sandbox.PrivateTarget
	dest   netip.AddrPort // zero for a named target
	up     net.Conn
}

// dropLocked forgets a connection and returns both its sides, to close at
// once after privMu is released: a sandbox side that already half-closed would
// otherwise leave the upstream waiting on the connector, and closing an
// upstream can block on the network. Called with privMu held.
func (p *liveProxy) dropLocked(c net.Conn) []net.Conn {
	out := []net.Conn{c}
	if pc := p.conns[c]; pc != nil && pc.up != nil {
		out = append(out, pc.up)
	}
	delete(p.conns, c)
	return out
}

func closeConns(cs []net.Conn) {
	for _, c := range cs {
		_ = c.Close()
	}
}

// privLive is keyed by the vm-layer sandbox id. Package-level for the same
// reason as tagMembers: teardownNetNS has no Manager in scope.
var (
	privMu   sync.Mutex
	privLive = map[string]*liveProxy{}
)

func privateOn(id string) bool {
	privMu.Lock()
	defer privMu.Unlock()
	return privLive[id] != nil
}

// privateLookup is what the namespace resolver answers for this sandbox.
func privateLookup(id string) map[string]string {
	privMu.Lock()
	defer privMu.Unlock()
	p := privLive[id]
	if p == nil {
		return nil
	}
	routes := make([]privateRoute, 0, len(p.routes))
	for _, r := range p.routes {
		routes = append(routes, r)
	}
	return privateDNS(routes)
}

// applyPrivate runs update on the sandbox's proxy under privMu, starts the
// proxy when that leaves it something to route and closes it when nothing is
// left, then re-renders the namespace on that change, or refills the set when
// update says its contents changed. update returns the connections to close.
func (m *Manager) applyPrivate(id, sandboxID string, needRoutes bool, update func(p *liveProxy) (dropped []net.Conn, refill bool)) error {
	m.mu.RLock()
	sb, ok := m.active[id]
	m.mu.RUnlock()
	if !ok {
		return ErrSandboxNotFound
	}
	if sb.NetNS == "" {
		return fmt.Errorf("%w: private access needs per-sandbox namespaces (FC_NETNS=1)", rt.ErrUnsupported)
	}
	if m.config.PrivateDial == nil {
		return fmt.Errorf("%w: this node has no control plane to reach connectors through", rt.ErrUnsupported)
	}
	if needRoutes && (m.config.PrivateDialDest == nil || m.config.PrivateDNS == nil) {
		return fmt.Errorf("%w: this node cannot carry subnet or DNS grants", rt.ErrUnsupported)
	}
	slot, _, okName := parseNetNSName(sb.NetNS)
	if !okName {
		return fmt.Errorf("namespace %q does not carry a slot", sb.NetNS)
	}

	privMu.Lock()
	p := privLive[id]
	was := p != nil
	if p == nil {
		p = &liveProxy{ns: sb.NetNS, sandboxID: sandboxID, routes: map[string]privateRoute{}, conns: map[net.Conn]*privConn{},
			gate: newPrivateGate(), learned: map[netip.Addr]learnedAddr{}, dialDest: m.config.PrivateDialDest, resolve: m.config.PrivateDNS}
	}
	dropped, refill := update(p)
	now := len(p.routes) > 0 || len(p.subnets) > 0 || len(p.suffixes) > 0
	switch {
	case now && !was:
		// Listening before the redirect exists, or the first connection is refused.
		l, err := listenTCPInNS(netnsRunDir+"/"+sb.NetNS, net.JoinHostPort(netnsTapGW, fmt.Sprint(privateProxyPort)))
		if err != nil {
			privMu.Unlock()
			closeConns(dropped)
			return fmt.Errorf("netns %s: private proxy: %w", sb.NetNS, err)
		}
		p.l = l
		privLive[id] = p
		go m.servePrivate(id, p)
	case !now && was:
		dropped = append(dropped, closeProxyLocked(id, p)...)
	}
	privMu.Unlock()
	closeConns(dropped)

	if was != now {
		if err := m.syncNamespace(id, sb, slot); err != nil {
			if now {
				stopPrivate(id)
			}
			return err
		}
		return nil
	}
	if now && refill {
		return refillPrivateSet(p)
	}
	return nil
}

// SetPrivate replaces the targets a sandbox may reach and applies them to the
// running sandbox. Emptying every target and route closes the proxy and every
// connection it holds. id is the vm-layer id; sandboxID is the daemon's, which
// dials carry.
func (m *Manager) SetPrivate(id, sandboxID string, ts []sandbox.PrivateTarget) error {
	if err := sandbox.ValidPrivateTargets(ts); err != nil {
		return err
	}
	routes := routesByIP(assignPrivate(ts))
	return m.applyPrivate(id, sandboxID, false, func(p *liveProxy) ([]net.Conn, bool) {
		p.routes = routes
		granted := make(map[sandbox.PrivateTarget]bool, len(routes))
		for _, r := range routes {
			granted[r.Target] = true
		}
		var dropped []net.Conn
		for c, pc := range p.conns {
			if !pc.dest.IsValid() && !granted[pc.target] {
				dropped = append(dropped, p.dropLocked(c)...)
			}
		}
		return dropped, false
	})
}

// SetPrivateRoutes replaces the subnets and split-DNS suffixes a sandbox
// routes through its connectors, replaces the set's contents, and closes
// connections whose destination nothing covers any more.
func (m *Manager) SetPrivateRoutes(id, sandboxID string, r sandbox.PrivateRoutes) error {
	if err := sandbox.ValidPrivateRoutes(r); err != nil {
		return err
	}
	parsed := make([]netip.Prefix, 0, len(r.Subnets))
	for _, s := range r.Subnets {
		parsed = append(parsed, netip.MustParsePrefix(s))
	}
	// Folded: nft refuses an interval set element that overlaps another.
	subnets := sandbox.CarvePlatform(parsed)
	suffixes := append([]string(nil), r.DNS...)
	return m.applyPrivate(id, sandboxID, !r.Empty(), func(p *liveProxy) ([]net.Conn, bool) {
		p.subnets, p.suffixes = subnets, suffixes
		if p.learned == nil {
			p.learned = map[netip.Addr]learnedAddr{}
		}
		for a, l := range p.learned {
			if !slices.Contains(suffixes, l.suffix) || coveredBy(subnets, a) {
				delete(p.learned, a)
			}
		}
		now := time.Now()
		var dropped []net.Conn
		for c, pc := range p.conns {
			if pc.dest.IsValid() && !coversDest(p.subnets, p.learned, pc.dest.Addr(), now) {
				dropped = append(dropped, p.dropLocked(c)...)
			}
		}
		return dropped, true
	})
}

// refillPrivateSet replaces the namespace's set with the proxy's contents.
func refillPrivateSet(p *liveProxy) error {
	p.setMu.Lock()
	defer p.setMu.Unlock()
	privMu.Lock()
	script, ns := privateSetRefill(privateSetElements(p.subnets, p.learned, time.Now())), p.ns
	privMu.Unlock()
	return applyRulesetInNS(netnsRunDir+"/"+ns, script)
}

// privateElements renders a running sandbox's set for a ruleset reload.
func privateElements(id string) string {
	privMu.Lock()
	defer privMu.Unlock()
	if p := privLive[id]; p != nil {
		return privateSetElements(p.subnets, p.learned, time.Now())
	}
	return ""
}

// lockPrivateSet holds a sandbox's set writes while its ruleset reloads, so an
// address learned meanwhile is in the rendered set or added after it.
func lockPrivateSet(id string) (unlock func()) {
	privMu.Lock()
	p := privLive[id]
	privMu.Unlock()
	if p == nil {
		return func() {}
	}
	p.setMu.Lock()
	return p.setMu.Unlock
}

// learnAddr routes an address a split-DNS answer named, before the answer goes
// back to the guest, and returns the TTL the guest may cache it for. false
// means the set could not take it.
func learnAddr(id, suffix string, a netip.Addr, answerTTL uint32) (uint32, bool) {
	privMu.Lock()
	p := privLive[id]
	privMu.Unlock()
	if p == nil {
		return 0, false
	}
	p.setMu.Lock()
	defer p.setMu.Unlock()
	now := time.Now()
	privMu.Lock()
	if privLive[id] != p {
		privMu.Unlock()
		return 0, false
	}
	if !slices.Contains(p.suffixes, suffix) {
		privMu.Unlock()
		return 0, false
	}
	if coveredBy(p.subnets, a) {
		privMu.Unlock()
		_, ttl := dnsTimeouts(answerTTL)
		return ttl, true
	}
	if p.learned == nil {
		p.learned = map[netip.Addr]learnedAddr{}
	}
	prev, known := p.learned[a]
	expires, ttl, add, refill := learnPlan(prev, known, now, answerTTL)
	if add && len(p.learned) >= maxLearnedPerSandbox {
		dropExpiredLearned(p.learned, now)
		if len(p.learned) >= maxLearnedPerSandbox {
			dropOldestLearned(p.learned)
			add, refill = false, true
		}
	}
	p.learned[a] = learnedAddr{suffix: suffix, expires: expires}
	var script string
	switch {
	case add:
		script = privateSetAdd(a, expires.Sub(now))
	case refill:
		script = privateSetRefill(privateSetElements(p.subnets, p.learned, now))
	}
	ns := p.ns
	privMu.Unlock()
	if script == "" {
		return ttl, true
	}
	if err := applyRulesetInNS(netnsRunDir+"/"+ns, script); err != nil {
		privMu.Lock()
		delete(p.learned, a)
		privMu.Unlock()
		return 0, false
	}
	return ttl, true
}

// dropOldestLearned removes the learned address nearest its expiry.
func dropOldestLearned(m map[netip.Addr]learnedAddr) {
	var oldest netip.Addr
	for a, l := range m {
		if !oldest.IsValid() || l.expires.Before(m[oldest].expires) {
			oldest = a
		}
	}
	delete(m, oldest)
}

// privateSplit is the resolver hook for a sandbox's granted suffixes: the
// lookup goes to the control plane and each answered address is routed first.
func privateSplit(id string) splitDNS {
	return func(qid uint16, q dnsmessage.Question) ([]byte, bool) {
		name := strings.TrimSuffix(strings.ToLower(q.Name.String()), ".")
		privMu.Lock()
		var (
			suffixes []string
			resolve  func(context.Context, string, string, string) (sandbox.PrivateDNSReply, error)
			sbID     string
		)
		if p := privLive[id]; p != nil {
			suffixes, resolve, sbID = p.suffixes, p.resolve, p.sandboxID
		}
		privMu.Unlock()
		suffix, ok := sandbox.LongestSuffix(name, suffixes)
		if !ok || resolve == nil {
			return nil, false
		}
		relay := func(qtype string) (sandbox.PrivateDNSReply, error) {
			ctx, cancel := context.WithTimeout(context.Background(), dnsForwardTimeout)
			defer cancel()
			return resolve(ctx, sbID, name, qtype)
		}
		learn := func(a netip.Addr, ttl uint32) (uint32, bool) { return learnAddr(id, suffix, a, ttl) }
		return splitAnswer(qid, q, relay, learn), true
	}
}

func (m *Manager) servePrivate(id string, p *liveProxy) {
	for {
		c, err := p.l.Accept()
		if err != nil {
			return
		}
		go m.proxyPrivate(id, p, c)
	}
}

func (m *Manager) proxyPrivate(id string, p *liveProxy, c net.Conn) {
	defer func() { _ = c.Close() }()
	dst, err := egressproxy.OriginalDst(c)
	if err != nil {
		return
	}
	privMu.Lock()
	r, named := privateRouteTo(p.routes, dst)
	var dest netip.AddrPort
	if a := dst.Addr().Unmap(); !named && coversDest(p.subnets, p.learned, a, time.Now()) {
		dest = netip.AddrPortFrom(a, dst.Port())
	}
	ok := (named || dest.IsValid()) && privLive[id] == p && p.gate.enter()
	if ok {
		p.conns[c] = &privConn{target: r.Target, dest: dest}
	}
	dialDest := p.dialDest
	privMu.Unlock()
	if !ok {
		return
	}
	defer func() {
		privMu.Lock()
		delete(p.conns, c)
		p.gate.leave()
		privMu.Unlock()
	}()
	var up net.Conn
	if dest.IsValid() {
		up, err = dialDest(context.Background(), p.sandboxID, dest)
	} else {
		up, err = m.config.PrivateDial(context.Background(), p.sandboxID, r.Target.Name, r.Target.Port)
	}
	if err != nil {
		m.log.Warn("private dial failed", zap.String("sandbox", p.sandboxID), zap.String("target", r.Target.Name), zap.Stringer("dest", dest), zap.Error(err))
		return
	}
	defer func() { _ = up.Close() }()
	privMu.Lock()
	pc := p.conns[c]
	if pc != nil {
		pc.up = up
	}
	privMu.Unlock()
	if pc == nil {
		return // revoked while dialling
	}
	privSplice(c, up)
}

// closeProxyLocked closes a proxy's listener and returns every connection it
// held, for the caller to close after releasing privMu. Called with privMu held.
func closeProxyLocked(id string, p *liveProxy) []net.Conn {
	_ = p.l.Close()
	var out []net.Conn
	for c := range p.conns {
		out = append(out, p.dropLocked(c)...)
	}
	delete(privLive, id)
	return out
}

func stopPrivate(id string) {
	var dropped []net.Conn
	privMu.Lock()
	if p := privLive[id]; p != nil {
		dropped = closeProxyLocked(id, p)
	}
	privMu.Unlock()
	closeConns(dropped)
}

// stopPrivateByNS closes the proxy living in a namespace that is going away.
func stopPrivateByNS(nsName string) {
	var dropped []net.Conn
	privMu.Lock()
	for id, p := range privLive {
		if p.ns == nsName {
			dropped = append(dropped, closeProxyLocked(id, p)...)
		}
	}
	privMu.Unlock()
	closeConns(dropped)
}

// guestLink is the tap's /16: the guest ARPs for any address in it instead of
// using its gateway.
var guestLink = netip.MustParsePrefix("192.168.0.0/16")

// privateLocalRoutes make the namespace own the private-target range, as
// `ip route add local 192.168.31.0/24 dev lo` does, and the rest of the guest
// link outside the platform ranges. The guest sees those addresses as on-link
// and ARPs for each one; without these routes nobody answers and the redirect
// never gets the packet, so a granted 192.168.x subnet or DNS answer is
// unreachable. Ungranted addresses there were unreachable already; they now
// get a reset from the namespace instead of an ARP timeout.
func privateLocalRoutes(loIndex int) []*netlink.Route {
	dsts := append([]netip.Prefix{netip.MustParsePrefix(privateSubnet)}, sandbox.CarvePlatform([]netip.Prefix{guestLink})...)
	rs := make([]*netlink.Route, len(dsts))
	for i, d := range dsts {
		rs[i] = &netlink.Route{
			LinkIndex: loIndex,
			Dst:       &net.IPNet{IP: d.Addr().AsSlice(), Mask: net.CIDRMask(d.Bits(), 32)},
			Type:      unix.RTN_LOCAL,
			Table:     unix.RT_TABLE_LOCAL,
			Scope:     netlink.SCOPE_HOST,
		}
	}
	return rs
}

// setPrivateLocalRoute adds or removes privateLocalRoutes in a namespace. Both
// are idempotent: an adopted namespace may already be in either state.
func setPrivateLocalRoute(nsPath string, on bool) error {
	ns, err := netns.GetFromPath(nsPath)
	if err != nil {
		return err
	}
	defer func() { _ = ns.Close() }()
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		return err
	}
	defer h.Close()
	lo, err := h.LinkByName("lo")
	if err != nil {
		return err
	}
	for _, r := range privateLocalRoutes(lo.Attrs().Index) {
		if on {
			err = h.RouteReplace(r)
		} else if err = h.RouteDel(r); errors.Is(err, unix.ESRCH) {
			err = nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}
