package vm

import (
	"fmt"
	"io"
	"net"
	"net/netip"
	"sort"
	"strings"

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
	return fmt.Sprintf("\t\tiifname %q ip daddr %s meta l4proto tcp redirect to :%d\n", tapName, privateSubnet, privateProxyPort)
}

func privateEgressDrop(on bool) string {
	if !on {
		return ""
	}
	return "\t\tip daddr " + privateSubnet + " drop\n"
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
