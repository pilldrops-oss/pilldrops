package httpx

import (
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
)

// ClientIP is the peer address to judge a request by: the first
// X-Forwarded-For hop when the connection itself came from a configured
// terminator, else the RemoteAddr host. Anyone can send the header, so
// trusting it unconditionally would let a stranger claim any source address —
// and this value keys the auth-failure throttle, the preview throttle, the
// agent-origin throttle and the preview source gate, as well as the address
// recorded against an agent token's last use.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" && TrustedProxy(host) {
		first, _, _ := strings.Cut(xff, ",")
		return strings.TrimSpace(first)
	}
	return host
}

// TrustedProxy reports whether a connection from host may set the client address.
func TrustedProxy(host string) bool {
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	for _, p := range TrustedProxyCIDRs() {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// TrustedProxyCIDRs parses TRUSTED_PROXY_CIDRS once. Unset means no proxy is
// trusted and X-Forwarded-For is ignored everywhere — which a caller that
// depends on the real client address has to warn about, since the gate then
// sees only the terminator.
var TrustedProxyCIDRs = sync.OnceValue(func() []netip.Prefix {
	var out []netip.Prefix
	for _, c := range strings.Split(os.Getenv("TRUSTED_PROXY_CIDRS"), ",") {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		p, err := netip.ParsePrefix(c)
		if err != nil {
			slog.Warn("ignoring an unparseable TRUSTED_PROXY_CIDRS entry; X-Forwarded-For from it is not trusted",
				"entry", c, "err", err)
			continue
		}
		out = append(out, p)
	}
	return out
})

// WarnIfUntrustedProxy logs once when per-address controls are in force and no
// proxy is trusted. Without TRUSTED_PROXY_CIDRS, ClientIP ignores
// X-Forwarded-For, so a tier sitting behind a TLS terminator sees the
// terminator's address on every request: each throttle collapses into one
// shared bucket and any address recorded for audit is the terminator's.
//
// Silence was the old behaviour and it is the wrong default for a control that
// looks like it is working. The caller passes whether anything per-address is
// actually switched on, because warning on a deployment with no throttles would
// train people to ignore it.
func WarnIfUntrustedProxy(tier string, perAddressControlsOn bool) {
	if !perAddressControlsOn || len(TrustedProxyCIDRs()) > 0 {
		return
	}
	slog.Warn("per-address limits are in force but TRUSTED_PROXY_CIDRS is unset: "+
		"X-Forwarded-For is ignored, so behind a TLS terminator every request presents "+
		"the terminator's own address and the limits share one bucket. Set it to the "+
		"terminator's networks", "component", tier)
}
