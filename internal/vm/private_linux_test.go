//go:build linux

package vm

import (
	"context"
	"net"
	"testing"

	"github.com/vishvananda/netlink"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
)

// The route must be the `ip route add local 192.168.31.0/24 dev lo` that made
// a target answer ARP on a live node, and nothing wider.
func TestPrivateLocalRouteOwnsOnlyTheTargetRange(t *testing.T) {
	r := privateLocalRoute(7)
	if r.Dst == nil || r.Dst.String() != privateSubnet {
		t.Fatalf("dst %v, want %s", r.Dst, privateSubnet)
	}
	if r.LinkIndex != 7 || r.Type != unix.RTN_LOCAL || r.Table != unix.RT_TABLE_LOCAL || r.Scope != netlink.SCOPE_HOST {
		t.Fatalf("route %+v is not a local route on lo", r)
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
