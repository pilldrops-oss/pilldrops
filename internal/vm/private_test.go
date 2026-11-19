package vm

import (
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
)

func TestAssignPrivateIsStable(t *testing.T) {
	t.Parallel()
	a := assignPrivate([]sandbox.PrivateTarget{{Name: "web", Port: 80}, {Name: "db", Port: 5432}})
	if len(a) != 2 || a[0].Target.Name != "db" || a[0].IP != "192.168.31.2" || a[1].IP != "192.168.31.3" {
		t.Fatalf("%+v", a)
	}
}

func TestPrivateDNSNames(t *testing.T) {
	t.Parallel()
	m := privateDNS(assignPrivate([]sandbox.PrivateTarget{{Name: "api", Host: "Internal.Corp", Port: 443}}))
	if m["api.private."] != "192.168.31.2" || m["internal.corp."] != "192.168.31.2" || len(m) != 2 {
		t.Fatalf("%v", m)
	}
}

func TestPrivateDNSSkipsInternalZoneHosts(t *testing.T) {
	t.Parallel()
	m := privateDNS(assignPrivate([]sandbox.PrivateTarget{{Name: "a", Host: "bravo.pd"}, {Name: "b", Host: "DB.Private."}}))
	if len(m) != 2 || m["a.private."] == "" || m["b.private."] == "" {
		t.Fatalf("%v", m)
	}
}

func TestRulesetPrivateRedirectBeforeDivert(t *testing.T) {
	t.Setenv("PILLDROPS_LOCAL_API_PORT", "")
	const divert = "\t\tiifname \"tap0\" tcp dport 443 redirect to :18443\n"
	rs := buildNetNSRuleset("vn7", "tap0", net.ParseIP("10.128.0.29"), net.ParseIP("10.128.0.30"), net.ParseIP("192.168.30.2"), "", divert, false, true)
	for _, want := range []string{
		`iifname "tap0" ip daddr 192.168.31.0/24 meta l4proto tcp redirect to :18445`,
		`iifname "tap0" udp dport 53 redirect to :53`,
		`ip daddr 192.168.31.0/24 drop`,
		"ip daddr 10.0.0.0/8 drop",
	} {
		if !strings.Contains(rs, want) {
			t.Errorf("missing %q in\n%s", want, rs)
		}
	}
	if strings.Index(rs, "192.168.31.0/24 drop") > strings.Index(rs, "dport { 53, 80, 443 }") {
		t.Error("UDP to private addresses must drop before the default accepts")
	}
	if strings.Index(rs, "redirect to :18445") > strings.Index(rs, "redirect to :18443") {
		t.Error("private redirect must come before the divert")
	}
	off := buildNetNSRuleset("vn7", "tap0", net.ParseIP("10.128.0.29"), net.ParseIP("10.128.0.30"), net.ParseIP("192.168.30.2"), "", "", false, false)
	if strings.Contains(off, "192.168.31") || strings.Contains(off, ":53") {
		t.Errorf("private off still renders private rules:\n%s", off)
	}
}

func TestPrivateProxyRoute(t *testing.T) {
	t.Parallel()
	routes := routesByIP(assignPrivate([]sandbox.PrivateTarget{{Name: "db", Port: 5432}}))
	for dst, want := range map[string]bool{
		"192.168.31.2:5432":  true,
		"192.168.31.2:5433":  false, // not the granted port
		"192.168.31.3:5432":  false, // nothing assigned there
		"192.168.30.1:18445": false, // the proxy itself, dialled directly
		"10.0.0.5:5432":      false,
	} {
		r, ok := privateRouteTo(routes, netip.MustParseAddrPort(dst))
		if ok != want || (ok && r.Target.Name != "db") {
			t.Errorf("%s: route %+v %v, want %v", dst, r, ok, want)
		}
	}
}

// A client that half-closes after its request must still get the reply: the
// upstream has no half-close, so the guest's FIN must not end it.
func TestPrivSpliceKeepsReplyAfterGuestHalfClose(t *testing.T) {
	t.Parallel()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	client, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	guest, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	up, far := net.Pipe()
	go func() {
		privSplice(guest, up)
		_ = guest.Close()
		_ = up.Close()
	}()
	go func() {
		buf := make([]byte, 4)
		if _, err := io.ReadFull(far, buf); err != nil || string(buf) != "ping" {
			t.Errorf("upstream read %q %v", buf, err)
		}
		time.Sleep(50 * time.Millisecond) // let the guest's FIN reach the splice first
		if _, err := far.Write([]byte("pong")); err != nil {
			t.Errorf("upstream closed before the reply: %v", err)
		}
		_ = far.Close()
	}()
	_, _ = client.Write([]byte("ping"))
	_ = client.(*net.TCPConn).CloseWrite()
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(client)
	if err != nil || string(got) != "pong" {
		t.Fatalf("client got %q %v", got, err)
	}
}

func TestPrivateGateLimits(t *testing.T) {
	t.Parallel()
	g := newPrivateGate()
	for i := range privateOpensPerSecond {
		if !g.enter() {
			t.Fatalf("open %d refused inside the burst", i)
		}
	}
	if g.enter() {
		t.Fatal("open past the per-second limit allowed")
	}

	g = newPrivateGate()
	g.opens = rate.NewLimiter(rate.Inf, 0)
	for range maxPrivateConnsPerSandbox {
		if !g.enter() {
			t.Fatal("refused below the concurrent limit")
		}
	}
	if g.enter() {
		t.Fatal("connection past the concurrent limit allowed")
	}
	g.leave()
	if !g.enter() {
		t.Fatal("a closed connection did not free its slot")
	}
}
