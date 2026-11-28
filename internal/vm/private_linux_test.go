//go:build linux

package vm

import (
	"context"
	"net"
	"net/netip"
	"testing"

	"github.com/vishvananda/netlink"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
)

// The routes own the private-target range and the guest link outside the
// platform ranges, never the guest's own /23 (its address and gateway).
func TestPrivateLocalRoutesOwnTheGuestLinkButNotTheGuest(t *testing.T) {
	rs := privateLocalRoutes(7)
	covers := func(ip string) bool {
		for _, r := range rs {
			if r.Dst.Contains(net.ParseIP(ip)) {
				return true
			}
		}
		return false
	}
	if rs[0].Dst.String() != privateSubnet {
		t.Fatalf("first route %v, want %s", rs[0].Dst, privateSubnet)
	}
	for _, ip := range []string{"192.168.31.2", "192.168.0.80", "192.168.1.1", "192.168.255.254"} {
		if !covers(ip) {
			t.Errorf("%s not owned by the namespace", ip)
		}
	}
	for _, ip := range []string{"192.168.30.1", "192.168.30.2", "10.0.0.1", "172.16.0.1"} {
		if covers(ip) {
			t.Errorf("%s owned by the namespace", ip)
		}
	}
	for _, r := range rs {
		if r.LinkIndex != 7 || r.Type != unix.RTN_LOCAL || r.Table != unix.RT_TABLE_LOCAL || r.Scope != netlink.SCOPE_HOST {
			t.Fatalf("route %+v is not a local route on lo", r)
		}
	}
}

// lockCheckConn records whether privMu was free when it was closed.
type lockCheckConn struct {
	net.Conn
	closed, lockFree bool
}

func (c *lockCheckConn) Close() error {
	if c.lockFree = privMu.TryLock(); c.lockFree {
		privMu.Unlock()
	}
	c.closed = true
	return nil
}

// Closing an upstream can block on the network; holding the node-wide lock
// meanwhile would stall every sandbox's private traffic.
func TestSetPrivateClosesRevokedConnsOutsideTheLock(t *testing.T) {
	db := sandbox.PrivateTarget{Name: "db", Port: 5432}
	api := sandbox.PrivateTarget{Name: "api", Port: 443}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	const id = "vm-lock"
	m := &Manager{
		active: map[string]*Sandbox{id: {ID: id, NetNS: netnsNameFor(1, id)}},
		config: Config{PrivateDial: func(context.Context, string, string, int) (net.Conn, error) { return nil, net.ErrClosed }},
		log:    zap.NewNop(),
	}
	guest, up, kept := &lockCheckConn{}, &lockCheckConn{}, &lockCheckConn{}
	privMu.Lock()
	privLive[id] = &liveProxy{ns: netnsNameFor(1, id), sandboxID: "sb-lock", l: l, gate: newPrivateGate(),
		routes: routesByIP(assignPrivate([]sandbox.PrivateTarget{api, db})),
		conns:  map[net.Conn]*privConn{guest: {target: db, up: up}, kept: {target: api}}}
	privMu.Unlock()
	t.Cleanup(func() { stopPrivate(id) })

	if err := m.SetPrivate(id, "sb-lock", []sandbox.PrivateTarget{api}); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]*lockCheckConn{"sandbox side": guest, "upstream": up} {
		if !c.closed || !c.lockFree {
			t.Fatalf("%s of the revoked target: closed %v, lock free %v", name, c.closed, c.lockFree)
		}
	}
	if kept.closed {
		t.Fatal("a still-granted connection was closed")
	}
	stopPrivate(id)
	if !kept.closed || !kept.lockFree {
		t.Fatalf("stop: closed %v, lock free %v", kept.closed, kept.lockFree)
	}
}

func TestLearnAddrRefusesARevokedSuffix(t *testing.T) {
	const id = "vm-learn-revoked"
	privMu.Lock()
	privLive[id] = &liveProxy{sandboxID: "sb-revoked", suffixes: []string{"other.internal"}, learned: map[netip.Addr]learnedAddr{}}
	p := privLive[id]
	privMu.Unlock()
	t.Cleanup(func() { privMu.Lock(); delete(privLive, id); privMu.Unlock() })
	if _, ok := learnAddr(id, "corp.internal", netip.MustParseAddr("198.51.100.7"), 60); ok {
		t.Fatal("an address was learned under a suffix no longer granted")
	}
	privMu.Lock()
	defer privMu.Unlock()
	if len(p.learned) != 0 {
		t.Fatalf("stored %v", p.learned)
	}
}
