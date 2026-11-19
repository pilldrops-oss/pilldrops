//go:build linux

package vm

import (
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
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
