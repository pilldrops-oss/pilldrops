//go:build linux

package vm

// Real namespaces and nft, no VM: run in a privileged Linux container, see the
// plan's Global Constraints. Skips anywhere else.

import (
	"context"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"golang.org/x/net/dns/dnsmessage"

	"github.com/pilldrops-oss/pilldrops/internal/egressproxy"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
)

func needNFT(t *testing.T) {
	t.Helper()
	if os.Getenv("PILLDROPS_NFT_TEST") != "1" || os.Geteuid() != 0 {
		t.Skip("SKIP (not a pass): set PILLDROPS_NFT_TEST=1 as root in a privileged Linux container")
	}
}

func sh(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return string(out)
}

// testNamespaces builds a sandbox namespace named as the node names one (tap0
// at 192.168.30.1/16 facing a guest namespace at .2/16, as on a node, vn0 facing a host namespace
// that owns the public test address 203.0.113.7) and loads the sandbox ruleset
// with private access on, policy as its egress tail.
func testNamespaces(t *testing.T, id, policy string) (sbNS, guestNS, hostNS string) {
	t.Helper()
	sfx := strconv.Itoa(os.Getpid())
	sbNS, guestNS, hostNS = netnsNameFor(1, id), "pdguest"+sfx, "pdhost"+sfx
	for _, ns := range []string{sbNS, guestNS, hostNS} {
		sh(t, "ip", "netns", "add", ns)
		t.Cleanup(func() { _ = exec.Command("ip", "netns", "del", ns).Run() })
	}
	sh(t, "ip", "link", "add", "tap0", "netns", sbNS, "type", "veth", "peer", "name", "eth0", "netns", guestNS)
	sh(t, "ip", "link", "add", "vn0", "netns", sbNS, "type", "veth", "peer", "name", "eth0", "netns", hostNS)
	in := func(ns string, args ...string) { sh(t, append([]string{"ip", "netns", "exec", ns}, args...)...) }
	for _, c := range [][]string{
		{"ip", "addr", "add", "192.168.30.1/16", "dev", "tap0"}, {"ip", "addr", "add", "10.128.0.30/30", "dev", "vn0"},
		{"ip", "link", "set", "tap0", "up"}, {"ip", "link", "set", "vn0", "up"}, {"ip", "link", "set", "lo", "up"},
		{"ip", "route", "add", "default", "via", "10.128.0.29"}, {"sysctl", "-qw", "net.ipv4.ip_forward=1"},
	} {
		in(sbNS, c...)
	}
	in(guestNS, "ip", "addr", "add", "192.168.30.2/16", "dev", "eth0")
	in(guestNS, "ip", "link", "set", "eth0", "up")
	in(guestNS, "ip", "route", "add", "default", "via", "192.168.30.1")
	in(hostNS, "ip", "addr", "add", "10.128.0.29/30", "dev", "eth0")
	in(hostNS, "ip", "addr", "add", "203.0.113.7/32", "dev", "eth0")
	in(hostNS, "ip", "link", "set", "eth0", "up")
	rs := buildNetNSRuleset("vn0", "tap0", net.ParseIP("10.128.0.29"), net.ParseIP("10.128.0.30"), net.ParseIP("192.168.30.2"), policy, "", false, true, "")
	if err := applyRulesetInNS(netnsRunDir+"/"+sbNS, rs); err != nil {
		t.Fatal(err)
	}
	return sbNS, guestNS, hostNS
}

func dialInNS(t *testing.T, ns, network, addr string) net.Conn {
	t.Helper()
	var c net.Conn
	if err := inNS(netnsRunDir+"/"+ns, func() (io.Closer, error) {
		var err error
		c, err = net.DialTimeout(network, addr, 2*time.Second)
		return c, err
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func listSet(t *testing.T, ns string) string {
	t.Helper()
	return sh(t, "ip", "netns", "exec", ns, "nft", "list", "set", "ip", "sandbox", privateSetName)
}

func TestPrivateSetRedirectsTCPAndDropsUDP(t *testing.T) {
	needNFT(t)
	t.Setenv("PILLDROPS_LOCAL_API_PORT", "")
	// Accept-all tail and a port other than 53: the default tail drops other
	// UDP ports and udp/53 goes to the namespace resolver, so either would hide the set.
	sbNS, guestNS, hostNS := testNamespaces(t, "vm-pdset", "\t\taccept")
	host, err := listenInNS(netnsRunDir+"/"+hostNS, "203.0.113.7:9999")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = host.Close() }()
	leaves := func() bool {
		_, _ = dialInNS(t, guestNS, "udp", "203.0.113.7:9999").Write([]byte("x"))
		_ = host.SetReadDeadline(time.Now().Add(time.Second))
		_, _, err := host.ReadFrom(make([]byte, 16))
		return err == nil
	}
	if !leaves() {
		t.Fatal("control: UDP to a public address did not leave with the set empty")
	}
	granted := []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}
	learned := map[netip.Addr]learnedAddr{netip.MustParseAddr("198.51.100.20"): {suffix: "corp.internal", expires: time.Now().Add(time.Minute)}}
	full := buildNetNSRuleset("vn0", "tap0", net.ParseIP("10.128.0.29"), net.ParseIP("10.128.0.30"), net.ParseIP("192.168.30.2"),
		"\t\taccept", "", false, true, privateSetElements(granted, learned, time.Now()))
	if err := applyRulesetInNS(netnsRunDir+"/"+sbNS, full); err != nil {
		t.Fatalf("full ruleset with set elements refused: %v\n%s", err, full)
	}
	if out := listSet(t, sbNS); !strings.Contains(out, "203.0.113.0/24") || !strings.Contains(out, "198.51.100.20") {
		t.Fatalf("reloaded set lacks its elements:\n%s", out)
	}
	if leaves() {
		t.Fatal("UDP to a granted range left the namespace directly")
	}
	l, err := listenTCPInNS(netnsRunDir+"/"+sbNS, "192.168.30.1:18445")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	go func() { // not dialInNS: t.Fatal must not run off the test goroutine
		_ = inNS(netnsRunDir+"/"+guestNS, func() (io.Closer, error) { return net.DialTimeout("tcp", "203.0.113.7:80", 2*time.Second) })
	}()
	_ = l.(*net.TCPListener).SetDeadline(time.Now().Add(3 * time.Second))
	c, err := l.Accept()
	if err != nil {
		t.Fatalf("TCP to the granted range never reached the proxy: %v", err)
	}
	defer func() { _ = c.Close() }()
	if dst, err := egressproxy.OriginalDst(c); err != nil || dst.String() != "203.0.113.7:80" {
		t.Fatalf("original destination %v %v", dst, err)
	}
	if err := applyRulesetInNS(netnsRunDir+"/"+sbNS, privateSetAdd(netip.MustParseAddr("198.51.100.9"), 2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if out := listSet(t, sbNS); !strings.Contains(out, "198.51.100.9") || !strings.Contains(out, "timeout") {
		t.Fatalf("learned address missing:\n%s", out)
	}
	time.Sleep(3 * time.Second)
	if out := listSet(t, sbNS); strings.Contains(out, "198.51.100.9") {
		t.Fatalf("learned address outlived its timeout:\n%s", out)
	}
}

// The guest's /16 makes a granted 192.168.x address on-link: the guest ARPs
// for it, and only the namespace's local routes make anything answer.
func TestGrantedGuestLinkAddressReachesTheProxy(t *testing.T) {
	needNFT(t)
	t.Setenv("PILLDROPS_LOCAL_API_PORT", "")
	sbNS, guestNS, _ := testNamespaces(t, "vm-pdlink", "\t\taccept")
	granted := []netip.Prefix{netip.MustParsePrefix("192.168.0.0/24")}
	rs := buildNetNSRuleset("vn0", "tap0", net.ParseIP("10.128.0.29"), net.ParseIP("10.128.0.30"), net.ParseIP("192.168.30.2"),
		"\t\taccept", "", false, true, privateSetElements(granted, nil, time.Now()))
	if err := applyRulesetInNS(netnsRunDir+"/"+sbNS, rs); err != nil {
		t.Fatal(err)
	}
	if err := setPrivateLocalRoute(netnsRunDir+"/"+sbNS, true); err != nil {
		t.Fatal(err)
	}
	l, err := listenTCPInNS(netnsRunDir+"/"+sbNS, "192.168.30.1:18445")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	go func() { // not dialInNS: t.Fatal must not run off the test goroutine
		_ = inNS(netnsRunDir+"/"+guestNS, func() (io.Closer, error) { return net.DialTimeout("tcp", "192.168.0.80:18999", 2*time.Second) })
	}()
	_ = l.(*net.TCPListener).SetDeadline(time.Now().Add(3 * time.Second))
	c, err := l.Accept()
	if err != nil {
		t.Fatalf("TCP to a granted 192.168.x address never reached the proxy: %v", err)
	}
	defer func() { _ = c.Close() }()
	if dst, err := egressproxy.OriginalDst(c); err != nil || dst.String() != "192.168.0.80:18999" {
		t.Fatalf("original destination %v %v", dst, err)
	}
	// The guest keeps its own link: the gateway still answers.
	if out := sh(t, "ip", "netns", "exec", guestNS, "ping", "-c1", "-W1", "192.168.30.1"); !strings.Contains(out, "1 received") {
		t.Fatalf("gateway unreachable with the local routes in place:\n%s", out)
	}
	if err := setPrivateLocalRoute(netnsRunDir+"/"+sbNS, false); err != nil {
		t.Fatal(err)
	}
	if out := sh(t, "ip", "netns", "exec", sbNS, "ip", "route", "show", "table", "local", "type", "local", "dev", "lo"); strings.Contains(out, "192.168.") {
		t.Fatalf("turning private access off left local routes:\n%s", out)
	}
}

// Review focus 2: the set holds the address by the time the guest has the answer.
func TestSplitDNSAddsBeforeReplying(t *testing.T) {
	needNFT(t)
	const id = "vm-pdsplit"
	sbNS, guestNS, _ := testNamespaces(t, id, "")
	privMu.Lock()
	privLive[id] = &liveProxy{ns: sbNS, sandboxID: "sb-split", routes: map[string]privateRoute{}, conns: map[net.Conn]*privConn{},
		gate: newPrivateGate(), suffixes: []string{"corp.internal"}, learned: map[netip.Addr]learnedAddr{},
		resolve: func(context.Context, string, string, string) (sandbox.PrivateDNSReply, error) {
			return sandbox.PrivateDNSReply{Answers: []sandbox.PrivateDNSAnswer{{Type: "A", Value: "198.51.100.7", TTL: 60}}}, nil
		}}
	privMu.Unlock()
	t.Cleanup(func() { privMu.Lock(); delete(privLive, id); privMu.Unlock() })
	pc, err := listenInNS(netnsRunDir+"/"+sbNS, "192.168.30.1:53")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()
	none := func() map[string]string { return nil }
	go serveDNS(pc, none, none, privateSplit(id), nil)
	c := dialInNS(t, guestNS, "udp", "192.168.30.1:53")
	_, _ = c.Write(query(t, "wiki.corp.internal.", dnsmessage.TypeA))
	buf := make([]byte, 1500)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := c.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if out := listSet(t, sbNS); !strings.Contains(out, "198.51.100.7") {
		t.Fatalf("the guest had the answer before the set had the address:\n%s", out)
	}
	if m := unpack(t, buf[:n]); len(m.Answers) != 1 || m.Answers[0].Header.TTL != 60 {
		t.Fatalf("reply %+v", m)
	}
}

func TestSetPrivateRoutesClosesWhatItNoLongerCovers(t *testing.T) {
	needNFT(t)
	const id = "vm-pdroutes"
	sbNS, _, _ := testNamespaces(t, id, "")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	none := func(context.Context, string, string, int) (net.Conn, error) { return nil, net.ErrClosed }
	m := &Manager{
		active: map[string]*Sandbox{id: {ID: id, NetNS: sbNS, TapName: "tap0", VMIP: "192.168.30.2"}},
		config: Config{PrivateDial: none,
			PrivateDialDest: func(context.Context, string, netip.AddrPort) (net.Conn, error) { return nil, net.ErrClosed },
			PrivateDNS:      func(context.Context, string, string, string) (sandbox.PrivateDNSReply, error) { return sandbox.PrivateDNSReply{}, nil }},
		log: zap.NewNop(),
	}
	near, far := &lockCheckConn{}, &lockCheckConn{}
	privMu.Lock()
	privLive[id] = &liveProxy{ns: sbNS, sandboxID: "sb-routes", l: l, routes: map[string]privateRoute{}, gate: newPrivateGate(),
		subnets: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")}, suffixes: []string{"corp.internal"},
		learned: map[netip.Addr]learnedAddr{netip.MustParseAddr("172.16.0.9"): {suffix: "corp.internal", expires: time.Now().Add(time.Minute)}},
		conns: map[net.Conn]*privConn{
			near: {dest: netip.MustParseAddrPort("10.0.5.7:80")},
			far:  {dest: netip.MustParseAddrPort("172.16.0.9:443")},
		}}
	privMu.Unlock()
	t.Cleanup(func() { stopPrivate(id) })
	if err := m.SetPrivateRoutes(id, "sb-routes", sandbox.PrivateRoutes{Subnets: []string{"10.0.5.0/24"}, DNS: []string{}}); err != nil {
		t.Fatal(err)
	}
	if near.closed || !far.closed || !far.lockFree {
		t.Fatalf("near closed %v, far closed %v (lock free %v)", near.closed, far.closed, far.lockFree)
	}
	if out := listSet(t, sbNS); !strings.Contains(out, "10.0.5.0/24") || strings.Contains(out, "10.0.0.0/16") || strings.Contains(out, "172.16.0.9") {
		t.Fatalf("set not replaced:\n%s", out)
	}
	if err := m.SetPrivateRoutes(id, "sb-routes", sandbox.PrivateRoutes{Subnets: []string{"10.0.0.0/16", "10.0.5.0/24"}}); err != nil {
		t.Fatalf("overlapping subnets must be folded before nft sees them: %v", err)
	}
	if out := listSet(t, sbNS); !strings.Contains(out, "10.0.0.0/16") {
		t.Fatalf("folded set:\n%s", out)
	}
}
