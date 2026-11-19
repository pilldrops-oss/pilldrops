//go:build linux

package vm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"go.uber.org/zap"
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
}

// privConn is one proxied connection: its target, and its upstream once dialled.
type privConn struct {
	target sandbox.PrivateTarget
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

// SetPrivate replaces the targets a sandbox may reach and applies them to the
// running sandbox. An empty list closes the proxy and every connection it
// holds. id is the vm-layer id; sandboxID is the daemon's, which dials carry.
func (m *Manager) SetPrivate(id, sandboxID string, ts []sandbox.PrivateTarget) error {
	if err := sandbox.ValidPrivateTargets(ts); err != nil {
		return err
	}
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
	slot, _, okName := parseNetNSName(sb.NetNS)
	if !okName {
		return fmt.Errorf("namespace %q does not carry a slot", sb.NetNS)
	}
	routes := assignPrivate(ts)

	var dropped []net.Conn
	privMu.Lock()
	p := privLive[id]
	was, now := p != nil, len(routes) > 0
	if now && p == nil {
		// Listening before the redirect exists, or the first connection is refused.
		l, err := listenTCPInNS(netnsRunDir+"/"+sb.NetNS, net.JoinHostPort(netnsTapGW, fmt.Sprint(privateProxyPort)))
		if err != nil {
			privMu.Unlock()
			return fmt.Errorf("netns %s: private proxy: %w", sb.NetNS, err)
		}
		p = &liveProxy{ns: sb.NetNS, sandboxID: sandboxID, l: l, conns: map[net.Conn]*privConn{}, gate: newPrivateGate()}
		privLive[id] = p
		go m.servePrivate(id, p)
	}
	if now {
		p.routes = routesByIP(routes)
		granted := make(map[sandbox.PrivateTarget]bool, len(routes))
		for _, r := range routes {
			granted[r.Target] = true
		}
		for c, pc := range p.conns {
			if !granted[pc.target] {
				dropped = append(dropped, p.dropLocked(c)...)
			}
		}
	} else if p != nil {
		dropped = closeProxyLocked(id, p)
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
	}
	return nil
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
	r, ok := privateRouteTo(p.routes, dst)
	ok = ok && privLive[id] == p && p.gate.enter()
	if ok {
		p.conns[c] = &privConn{target: r.Target}
	}
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
	up, err := m.config.PrivateDial(context.Background(), p.sandboxID, r.Target.Name, r.Target.Port)
	if err != nil {
		m.log.Warn("private dial failed", zap.String("sandbox", p.sandboxID), zap.String("target", r.Target.Name), zap.Error(err))
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

// privateLocalRoute makes the namespace own the whole target range, as
// `ip route add local 192.168.31.0/24 dev lo` does. The guest sees the range
// as on-link through the tap's /16 and ARPs for each target; without this
// nobody answers and the redirect never gets the packet.
func privateLocalRoute(loIndex int) *netlink.Route {
	_, dst, _ := net.ParseCIDR(privateSubnet)
	return &netlink.Route{
		LinkIndex: loIndex,
		Dst:       dst,
		Type:      unix.RTN_LOCAL,
		Table:     unix.RT_TABLE_LOCAL,
		Scope:     netlink.SCOPE_HOST,
	}
}

// setPrivateLocalRoute adds or removes privateLocalRoute in a namespace. Both
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
	r := privateLocalRoute(lo.Attrs().Index)
	if on {
		return h.RouteReplace(r)
	}
	if err := h.RouteDel(r); err != nil && !errors.Is(err, unix.ESRCH) {
		return err
	}
	return nil
}
