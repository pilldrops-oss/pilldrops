//go:build linux

package vm

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"

	"go.uber.org/zap"

	"github.com/pilldrops-oss/pilldrops/internal/egressproxy"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
)

// liveProxy is one sandbox's private proxy: the listener on its tap gateway,
// the routes it may open, and the sandbox-side connections it holds open.
type liveProxy struct {
	ns     string
	l      net.Listener
	routes map[string]privateRoute            // by IP
	conns  map[net.Conn]sandbox.PrivateTarget // open sandbox side -> its target
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
// running sandbox. An empty list closes the proxy and every connection it holds.
func (m *Manager) SetPrivate(id string, ts []sandbox.PrivateTarget) error {
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
		p = &liveProxy{ns: sb.NetNS, l: l, conns: map[net.Conn]sandbox.PrivateTarget{}}
		privLive[id] = p
		go m.servePrivate(id, p)
	}
	if now {
		p.routes = routesByIP(routes)
		granted := make(map[sandbox.PrivateTarget]bool, len(routes))
		for _, r := range routes {
			granted[r.Target] = true
		}
		for c, t := range p.conns {
			if !granted[t] {
				_ = c.Close()
			}
		}
	} else if p != nil {
		closeProxyLocked(id, p)
	}
	privMu.Unlock()

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
	ok = ok && privLive[id] == p
	if ok {
		p.conns[c] = r.Target
	}
	privMu.Unlock()
	if !ok {
		return
	}
	defer func() {
		privMu.Lock()
		delete(p.conns, c)
		privMu.Unlock()
	}()
	up, err := m.config.PrivateDial(context.Background(), id, r.Target.Name, r.Target.Port)
	if err != nil {
		m.log.Debug("private dial failed", zap.String("sandbox", id), zap.String("target", r.Target.Name), zap.Error(err))
		return
	}
	defer func() { _ = up.Close() }()
	privSplice(c, up)
}

// privSplice copies both ways. The upstream carries no half-close, so the
// sandbox finishing its side ends the upstream; the upstream finishing only
// half-closes the sandbox side, so a reply already sent still arrives.
func privSplice(guest, up net.Conn) {
	done := make(chan struct{}, 2)
	half := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if t, ok := dst.(*net.TCPConn); ok {
			_ = t.CloseWrite()
		} else {
			_ = dst.Close()
		}
		done <- struct{}{}
	}
	go half(guest, up)
	go half(up, guest)
	<-done
	<-done
}

// closeProxyLocked closes a proxy's listener and every connection it holds.
// Called with privMu held.
func closeProxyLocked(id string, p *liveProxy) {
	_ = p.l.Close()
	for c := range p.conns {
		_ = c.Close()
	}
	delete(privLive, id)
}

func stopPrivate(id string) {
	privMu.Lock()
	defer privMu.Unlock()
	if p := privLive[id]; p != nil {
		closeProxyLocked(id, p)
	}
}

// stopPrivateByNS closes the proxy living in a namespace that is going away.
func stopPrivateByNS(nsName string) {
	privMu.Lock()
	defer privMu.Unlock()
	for id, p := range privLive {
		if p.ns == nsName {
			closeProxyLocked(id, p)
		}
	}
}
