//go:build linux

package vm

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
)

// TestPrivateProxyLive boots a real sandbox with FC_NETNS, grants one target
// served by an in-process echo dialer, and checks name, port and revoke. Run
// like the other Live tests.
func TestPrivateProxyLive(t *testing.T) {
	cfg, tmpl := liveCfg(t)
	cfg.NetNS = true
	cfg.Templates = []string{tmpl}
	dials := make(chan string, 8)
	upClosed := make(chan struct{}, 8)
	cfg.PrivateDial = func(_ context.Context, sandboxID, target string, _ int) (net.Conn, error) {
		if sandboxID != "sb-platform" {
			t.Errorf("dialled as %q, want the platform id", sandboxID)
		}
		dials <- target
		a, b := net.Pipe()
		go func() {
			_, _ = io.Copy(b, b)
			upClosed <- struct{}{}
		}()
		return a, nil
	}
	ctx := context.Background()
	m := liveManager(t, cfg)
	sb, err := m.Create(ctx, CreateRequest{Template: tmpl, UserID: "live-test"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := sb.ID
	defer func() { _ = m.Destroy(ctx, id) }()
	run := func(cmd string) string {
		t.Helper()
		res, err := m.Exec(ctx, id, cmd, "", 30*time.Second)
		if err != nil {
			t.Fatalf("exec %q: %v", cmd, err)
		}
		return string(res.Stdout) + string(res.Stderr)
	}
	nextDial := func() string {
		t.Helper()
		select {
		case got := <-dials:
			return got
		case <-time.After(5 * time.Second):
			t.Fatal("no dial reached the control plane")
			return ""
		}
	}

	m.mu.RLock()
	nsPath := netnsRunDir + "/" + m.active[id].NetNS
	m.mu.RUnlock()
	hasLocalRoute := func() bool {
		t.Helper()
		ns, err := netns.GetFromPath(nsPath)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ns.Close() }()
		h, err := netlink.NewHandleAt(ns)
		if err != nil {
			t.Fatal(err)
		}
		defer h.Close()
		filter := &netlink.Route{Table: unix.RT_TABLE_LOCAL, Dst: privateLocalRoutes(0)[0].Dst}
		rs, err := h.RouteListFiltered(netlink.FAMILY_V4, filter, netlink.RT_FILTER_TABLE|netlink.RT_FILTER_DST)
		if err != nil {
			t.Fatal(err)
		}
		return len(rs) == 1 && rs[0].Type == unix.RTN_LOCAL
	}

	if err := m.SetPrivate(id, "sb-platform", []sandbox.PrivateTarget{{Name: "db", Port: 5432}}); err != nil {
		t.Fatal(err)
	}
	if !hasLocalRoute() {
		t.Fatal("granting left the namespace without the local route for the target range")
	}
	if out := run(`getent hosts db.private; echo hi | nc -w 2 db.private 5432`); !strings.Contains(out, "192.168.31.2") || !strings.Contains(out, "hi") {
		t.Fatalf("guest output:\n%s", out)
	}
	if got := nextDial(); got != "db" {
		t.Fatalf("dialled %q", got)
	}
	<-upClosed

	// The redirect accepts any port in the range, so a refused port shows as
	// a closed connection that never reaches the control plane.
	if out := run(`echo hi | nc -w 2 db.private 5433; echo rc=$?`); strings.Contains(out, "hi") {
		t.Fatalf("wrong port was proxied:\n%s", out)
	}
	if out := run(`echo hi | nc -w 2 192.168.30.1 18445; echo rc=$?`); strings.Contains(out, "hi") {
		t.Fatalf("the proxy answered a direct dial:\n%s", out)
	}
	select {
	case got := <-dials:
		t.Fatalf("a refused connection dialled %q", got)
	default:
	}
	if out := run(`nc -z -w 2 10.0.0.5 5432; echo rc=$?`); !strings.Contains(out, "rc=1") {
		t.Fatalf("private range reachable directly:\n%s", out)
	}
	if out := run(`getent hosts nope.private; echo rc=$?`); !strings.Contains(out, "rc=2") {
		t.Fatalf("unknown .private resolved:\n%s", out)
	}

	// A connection held open across the revoke must be closed by it.
	go func() { _, _ = m.Exec(ctx, id, `sleep 20 | nc db.private 5432`, "", 30*time.Second) }()
	nextDial()
	if err := m.SetPrivate(id, "sb-platform", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-upClosed:
	case <-time.After(time.Second):
		t.Fatal("revoke left the open connection running")
	}
	if out := run(`getent hosts db.private; echo rc=$?`); !strings.Contains(out, "rc=2") {
		t.Fatalf("revoked name still resolves:\n%s", out)
	}
	if hasLocalRoute() {
		t.Fatal("revoking left the local route for the target range")
	}
}
