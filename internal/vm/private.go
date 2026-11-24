package vm

import (
	"fmt"
	"io"
	"maps"
	"net"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
)

const (
	privateProxyPort = 18445
	privateSubnet    = "192.168.31.0/24"
	// The control plane holds the same limits; refusing here keeps a connect
	// loop in one sandbox from reaching it at all.
	maxPrivateConnsPerSandbox = 100
	privateOpensPerSecond     = 50
)

// privateGate is one sandbox's private connection limits on the node. Not
// safe for concurrent use: callers hold privMu.
type privateGate struct {
	open  int
	opens *rate.Limiter
}

func newPrivateGate() *privateGate {
	return &privateGate{opens: rate.NewLimiter(privateOpensPerSecond, privateOpensPerSecond)}
}

// enter takes a connection slot and an open token, or reports false.
func (g *privateGate) enter() bool {
	if g.open >= maxPrivateConnsPerSandbox || !g.opens.Allow() {
		return false
	}
	g.open++
	return true
}

func (g *privateGate) leave() { g.open-- }

// privateRoute is one granted target at its sandbox-local address. The
// 192.168.31.0/24 range exists only inside the sandbox's own namespace.
type privateRoute struct {
	Target sandbox.PrivateTarget
	IP     string
}

func assignPrivate(ts []sandbox.PrivateTarget) []privateRoute {
	sorted := append([]sandbox.PrivateTarget(nil), ts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	out := make([]privateRoute, len(sorted))
	for i, t := range sorted {
		out[i] = privateRoute{Target: t, IP: fmt.Sprintf("192.168.31.%d", i+2)}
	}
	return out
}

func privateDNS(routes []privateRoute) map[string]string {
	m := make(map[string]string, 2*len(routes))
	for _, r := range routes {
		m[r.Target.Name+".private."] = r.IP
		host := strings.ToLower(strings.TrimSuffix(r.Target.Host, "."))
		// The orchestrator refuses these too; a host here must never shadow a peer or target name.
		if host != "" && !strings.HasSuffix(host, ".pd") && !strings.HasSuffix(host, ".private") {
			m[host+"."] = r.IP
		}
	}
	return m
}

func privateRedirect(on bool, tapName string) string {
	if !on {
		return ""
	}
	return fmt.Sprintf("\t\tiifname %q ip daddr %s meta l4proto tcp redirect to :%d\n", tapName, privateSubnet, privateProxyPort) +
		fmt.Sprintf("\t\tiifname %q ip daddr @%s meta l4proto tcp redirect to :%d\n", tapName, privateSetName, privateProxyPort)
}

// privateEgressDrop keeps UDP to a private address from leaving directly: a
// customer may announce a public range. UDP through connectors is phase 3.
func privateEgressDrop(on bool) string {
	if !on {
		return ""
	}
	return "\t\tip daddr " + privateSubnet + " drop\n\t\tip daddr @" + privateSetName + " drop\n"
}

var privatePrefix = netip.MustParsePrefix(privateSubnet)

func routesByIP(routes []privateRoute) map[string]privateRoute {
	m := make(map[string]privateRoute, len(routes))
	for _, r := range routes {
		m[r.IP] = r
	}
	return m
}

// privateRouteTo is what a connection the guest made to dst may reach. The
// guest can dial the proxy's own address directly, so the original
// destination must be a granted address and port, never anything else.
func privateRouteTo(routes map[string]privateRoute, dst netip.AddrPort) (privateRoute, bool) {
	if !privatePrefix.Contains(dst.Addr().Unmap()) {
		return privateRoute{}, false
	}
	r, ok := routes[dst.Addr().Unmap().String()]
	if !ok || int(dst.Port()) != r.Target.Port {
		return privateRoute{}, false
	}
	return r, true
}

// privSplice copies both ways. A clean EOF half-closes the side that can take
// it (the sandbox's TCP socket) and leaves the upstream, which has no
// half-close, open for the reply; the owner closes both once both directions
// are done. An error on either side closes both.
// ponytail: a sandbox that half-closes and a connector that never answers
// hold the upstream until either side closes or the grant is revoked.
func privSplice(guest, up net.Conn) {
	done := make(chan struct{}, 2)
	half := func(dst, src net.Conn) {
		if _, err := io.Copy(dst, src); err != nil {
			_ = guest.Close()
			_ = up.Close()
		} else if hc, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = hc.CloseWrite()
		}
		done <- struct{}{}
	}
	go half(guest, up)
	go half(up, guest)
	<-done
	<-done
}

const (
	privateSetName       = "private_dst"
	dnsMinTimeout        = 30 * time.Second
	dnsMaxTimeout        = 300 * time.Second
	maxLearnedPerSandbox = 1024
)

// learnedAddr is an address a split-DNS answer routes through a connector.
type learnedAddr struct {
	suffix  string
	expires time.Time
}

func coveredBy(ps []netip.Prefix, a netip.Addr) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// coversDest reports whether a connection to a goes through the connector: a
// granted subnet holds it, or a split-DNS answer that has not expired.
func coversDest(subnets []netip.Prefix, learned map[netip.Addr]learnedAddr, a netip.Addr, now time.Time) bool {
	if coveredBy(subnets, a) {
		return true
	}
	l, ok := learned[a]
	return ok && l.expires.After(now)
}

// privateSetElements renders the set's contents: the static subnets, then each
// live learned address with the whole seconds it has left. An address inside a
// subnet is left out: it is covered already, and nft refuses an interval
// element inside another.
func privateSetElements(subnets []netip.Prefix, learned map[netip.Addr]learnedAddr, now time.Time) string {
	els := make([]string, 0, len(subnets)+len(learned))
	for _, p := range subnets {
		els = append(els, p.String())
	}
	for _, a := range slices.SortedFunc(maps.Keys(learned), netip.Addr.Compare) {
		left := learned[a].expires.Sub(now) / time.Second
		if left < 1 || coveredBy(subnets, a) {
			continue
		}
		els = append(els, fmt.Sprintf("%s timeout %ds", a, left))
	}
	return strings.Join(els, ", ")
}

// privateSetDecl declares the set inside table ip sandbox, holding elements.
func privateSetDecl(elements string) string {
	out := "\tset " + privateSetName + " {\n\t\ttype ipv4_addr\n\t\tflags interval, timeout\n"
	if elements != "" {
		out += "\t\telements = { " + elements + " }\n"
	}
	return out + "\t}\n"
}

// privateSetRefill replaces the set's contents in one atomic nft batch.
func privateSetRefill(elements string) string {
	out := "flush set ip sandbox " + privateSetName + "\n"
	if elements != "" {
		out += "add element ip sandbox " + privateSetName + " { " + elements + " }\n"
	}
	return out
}

// privateSetAdd adds one learned address that is not in the set yet.
func privateSetAdd(a netip.Addr, timeout time.Duration) string {
	return fmt.Sprintf("add element ip sandbox %s { %s timeout %ds }\n", privateSetName, a, int(timeout/time.Second))
}

// dnsTimeouts turns an answer's TTL into the set timeout, clamped to 30-300 s,
// and the TTL the guest is told, never longer than the set keeps the address.
func dnsTimeouts(ttl uint32) (time.Duration, uint32) {
	timeout := max(dnsMinTimeout, min(time.Duration(ttl)*time.Second, dnsMaxTimeout))
	return timeout, min(ttl, uint32(dnsMaxTimeout/time.Second))
}

// learnPlan says how an answered address enters the set. Not there: add it.
// There with under half the new timeout left: refill the whole set, since an
// nft add does not extend an element that exists. Otherwise leave it, and tell
// the guest no more than the time it has left.
func learnPlan(prev learnedAddr, known bool, now time.Time, answerTTL uint32) (expires time.Time, ttl uint32, add, refill bool) {
	timeout, ttl := dnsTimeouts(answerTTL)
	if !known {
		return now.Add(timeout), ttl, true, false
	}
	left := prev.expires.Sub(now)
	if left < timeout/2 {
		return now.Add(timeout), ttl, false, true
	}
	return prev.expires, min(ttl, uint32(left/time.Second)), false, false
}
