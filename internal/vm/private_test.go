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
	rs := buildNetNSRuleset("vn7", "tap0", net.ParseIP("10.128.0.29"), net.ParseIP("10.128.0.30"), net.ParseIP("192.168.30.2"), "", divert, false, true, "")
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
	off := buildNetNSRuleset("vn7", "tap0", net.ParseIP("10.128.0.29"), net.ParseIP("10.128.0.30"), net.ParseIP("192.168.30.2"), "", "", false, false, "")
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

func TestPrivateSetElements(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0)
	subnets := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")}
	learned := map[netip.Addr]learnedAddr{
		netip.MustParseAddr("172.16.0.9"):  {suffix: "corp.internal", expires: now.Add(120 * time.Second)},
		netip.MustParseAddr("172.16.0.8"):  {suffix: "corp.internal", expires: now.Add(-time.Second)},     // expired
		netip.MustParseAddr("10.0.5.5"):    {suffix: "corp.internal", expires: now.Add(60 * time.Second)}, // inside a subnet: nft refuses overlapping intervals
		netip.MustParseAddr("172.16.0.10"): {suffix: "corp.internal", expires: now.Add(30*time.Second + 400*time.Millisecond)},
	}
	if got, want := privateSetElements(subnets, learned, now), "10.0.0.0/16, 172.16.0.9 timeout 120s, 172.16.0.10 timeout 30s"; got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if privateSetElements(nil, nil, now) != "" {
		t.Fatal("an empty set rendered elements")
	}
	if got := privateSetRefill(""); got != "flush set ip sandbox private_dst\n" {
		t.Fatalf("empty refill %q", got)
	}
	if got := privateSetAdd(netip.MustParseAddr("172.16.0.9"), 90*time.Second); got != "add element ip sandbox private_dst { 172.16.0.9 timeout 90s }\n" {
		t.Fatalf("add %q", got)
	}
}

func TestRulesetRendersThePrivateSet(t *testing.T) {
	t.Setenv("PILLDROPS_LOCAL_API_PORT", "")
	rs := buildNetNSRuleset("vn7", "tap0", net.ParseIP("10.128.0.29"), net.ParseIP("10.128.0.30"), net.ParseIP("192.168.30.2"), "", "", false, true, "10.0.0.0/16")
	for _, want := range []string{
		"set private_dst {",
		"flags interval, timeout",
		"elements = { 10.0.0.0/16 }",
		`iifname "tap0" ip daddr @private_dst meta l4proto tcp redirect to :18445`,
		"ip daddr @private_dst drop",
	} {
		if !strings.Contains(rs, want) {
			t.Errorf("missing %q in\n%s", want, rs)
		}
	}
	// UDP to a granted range must never leave directly: the drop sits above the accepts.
	if strings.Index(rs, "@private_dst drop") > strings.Index(rs, "dport { 53, 80, 443 }") {
		t.Error("the set's drop comes after the default accepts")
	}
	if strings.Contains(buildNetNSRuleset("vn7", "tap0", net.ParseIP("10.128.0.29"), net.ParseIP("10.128.0.30"), net.ParseIP("192.168.30.2"), "", "", false, false, ""), "private_dst") {
		t.Error("private off still renders the set")
	}
}

func TestLearnPlan(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0)
	if exp, ttl, add, refill := learnPlan(learnedAddr{}, false, now, 60); !add || refill || ttl != 60 || !exp.Equal(now.Add(60*time.Second)) {
		t.Errorf("unknown: %v %d %v %v", exp, ttl, add, refill)
	}
	fresh := learnedAddr{expires: now.Add(200 * time.Second)}
	if exp, ttl, add, refill := learnPlan(fresh, true, now, 300); add || refill || ttl != 200 || !exp.Equal(fresh.expires) {
		t.Errorf("fresh: the guest may not cache past the set's own expiry: %v %d %v %v", exp, ttl, add, refill)
	}
	stale := learnedAddr{expires: now.Add(20 * time.Second)}
	if exp, _, add, refill := learnPlan(stale, true, now, 300); add || !refill || !exp.Equal(now.Add(300*time.Second)) {
		t.Errorf("stale: %v %v %v", exp, add, refill)
	}
	for _, c := range []struct {
		ttl      uint32
		timeout  time.Duration
		replyTTL uint32
	}{{5, 30 * time.Second, 5}, {3600, 300 * time.Second, 300}, {120, 120 * time.Second, 120}} {
		if to, reply := dnsTimeouts(c.ttl); to != c.timeout || reply != c.replyTTL {
			t.Errorf("dnsTimeouts(%d) = %v %d", c.ttl, to, reply)
		}
	}
}

func TestCoversDest(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0)
	subnets := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")}
	learned := map[netip.Addr]learnedAddr{
		netip.MustParseAddr("172.16.0.9"): {expires: now.Add(time.Minute)},
		netip.MustParseAddr("172.16.0.8"): {expires: now},
	}
	for a, want := range map[string]bool{"10.0.5.7": true, "172.16.0.9": true, "172.16.0.8": false, "192.168.30.1": false} {
		if got := coversDest(subnets, learned, netip.MustParseAddr(a), now); got != want {
			t.Errorf("%s: %v", a, got)
		}
	}
}
