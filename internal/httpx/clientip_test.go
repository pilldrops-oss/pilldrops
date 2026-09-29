package httpx

import (
	"net/http/httptest"
	"net/netip"
	"testing"
)

// A rate limiter keyed on a header anyone can set is not a limiter: a caller
// rotating X-Forwarded-For gets a fresh bucket on every request.
func TestForwardedForIsIgnoredFromAnUntrustedPeer(t *testing.T) {
	r := httptest.NewRequest("GET", "/preview/sb1/8000/", nil)
	r.RemoteAddr = "203.0.113.9:4444"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := ClientIP(r); got != "203.0.113.9" {
		t.Fatalf("ClientIP = %q, want the real peer: a stranger claimed a source address", got)
	}
}

func TestForwardedForIsTakenFromAConfiguredTerminator(t *testing.T) {
	prev := TrustedProxyCIDRs
	t.Cleanup(func() { TrustedProxyCIDRs = prev })
	TrustedProxyCIDRs = func() []netip.Prefix { return []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")} }

	r := httptest.NewRequest("GET", "/preview/sb1/8000/", nil)
	r.RemoteAddr = "10.1.2.3:4444"
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 10.1.2.3")
	if got := ClientIP(r); got != "1.2.3.4" {
		t.Fatalf("ClientIP = %q, want the first hop behind the terminator", got)
	}
}
