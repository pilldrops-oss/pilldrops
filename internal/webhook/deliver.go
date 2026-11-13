// Package webhook delivers sandbox lifecycle events to customer-supplied URLs.
// The server dials an address the customer chose, so delivery is SSRF-guarded
// and signed over the timestamp.
package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// SignatureHeader carries the HMAC.
const SignatureHeader = "X-Pilldrops-Signature"

// TimestampHeader carries the signing timestamp as unix seconds.
const TimestampHeader = "X-Pilldrops-Timestamp"

// DeliveryIDHeader lets a receiver dedupe; retries reuse the same id.
const DeliveryIDHeader = "X-Pilldrops-Delivery"

// MaxSkew is how much clock difference a receiver should tolerate. Part of the
// contract, since the signature covers the timestamp.
const MaxSkew = 5 * time.Minute

// Sign returns the HMAC over timestamp + "." + body. Binding the timestamp bounds
// replay to MaxSkew; the separator keeps (ts="1", body="23") and (ts="12", body="3") distinct.
func Sign(secret string, ts time.Time, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts.Unix(), 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify checks a signature and the timestamp's freshness in constant time.
func Verify(secret, signature string, tsUnix int64, body []byte, now time.Time) error {
	ts := time.Unix(tsUnix, 0)
	// Both directions; a future timestamp would buy a forger unlimited validity.
	if d := now.Sub(ts); d > MaxSkew || d < -MaxSkew {
		return fmt.Errorf("timestamp is %s away from now, outside the %s window", d.Truncate(time.Second), MaxSkew)
	}
	want := Sign(secret, ts, body)
	if !hmac.Equal([]byte(want), []byte(signature)) {
		return errors.New("signature does not match")
	}
	return nil
}

// ErrBlockedTarget is returned for a URL that must not be dialed.
var ErrBlockedTarget = errors.New("webhook target is not allowed")

// cgNAT is RFC 6598 shared address space, which net.IP.IsPrivate does not cover
// and cloud providers route internal endpoints through.
var cgNAT = func() *net.IPNet { _, n, _ := net.ParseCIDR("100.64.0.0/10"); return n }()

// blockedIP reports whether an address must never be dialed for a customer-supplied URL.
func blockedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() ||
		cgNAT.Contains(ip)
}

// ResolveAndPin validates a webhook URL and returns the IPs it may be dialed on.
// Dialing only these closes DNS rebinding, where the name resolves public for the
// check and private for the dial. TLS still verifies against the hostname.
func ResolveAndPin(raw string) (u *url.URL, ips []net.IP, err error) {
	u, err = url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrBlockedTarget, err)
	}
	if u.Scheme != "https" {
		// The signature proves origin, not confidentiality.
		return nil, nil, fmt.Errorf("%w: must use https (got %q)", ErrBlockedTarget, u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return nil, nil, fmt.Errorf("%w: no host", ErrBlockedTarget)
	}

	if ip := net.ParseIP(host); ip != nil {
		if blockedIP(ip) {
			return nil, nil, fmt.Errorf("%w: %s is not a public address", ErrBlockedTarget, ip)
		}
		return u, []net.IP{ip}, nil
	}

	addrs, err := lookupIPs(host)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: cannot resolve %q", ErrBlockedTarget, host)
	}
	for _, ip := range addrs {
		// Any blocked address disqualifies the host; mixed public and private is the attack signature.
		if blockedIP(ip) {
			return nil, nil, fmt.Errorf("%w: %q resolves to %s, which is not public",
				ErrBlockedTarget, host, ip)
		}
		ips = append(ips, ip)
	}
	if len(ips) == 0 {
		return nil, nil, fmt.Errorf("%w: %q resolved to nothing usable", ErrBlockedTarget, host)
	}
	return u, ips, nil
}

// PinnedClient builds a client that will only reach the given addresses.
// Redirects are refused; every hop is another rebinding chance.
func PinnedClient(ips []net.IP, timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: timeout}
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return fmt.Errorf("%w: webhook targets must not redirect", ErrBlockedTarget)
		},
		Transport: &http.Transport{
			// An environment proxy would bypass the pinned address.
			Proxy: nil,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				_, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				var lastErr error
				for _, ip := range ips {
					// Re-checked here so an unvalidated list still cannot reach a private address.
					if blockedIP(ip) {
						lastErr = fmt.Errorf("%w: %s", ErrBlockedTarget, ip)
						continue
					}
					c, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
					if err == nil {
						return c, nil
					}
					lastErr = err
				}
				if lastErr == nil {
					lastErr = fmt.Errorf("%w: no dialable address", ErrBlockedTarget)
				}
				return nil, lastErr
			},
		},
	}
}

// DeliveryTimeout bounds one attempt.
const DeliveryTimeout = 5 * time.Second

// lookupIPs resolves host with a deadline; net.LookupIP has none and a hostile
// DNS server could stall delivery.
func lookupIPs(host string) ([]net.IP, error) {
	ctx, cancel := context.WithTimeout(context.Background(), webhookDNSTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP)
	}
	return ips, nil
}

// webhookDNSTimeout bounds hostname resolution for a webhook target.
const webhookDNSTimeout = 5 * time.Second
