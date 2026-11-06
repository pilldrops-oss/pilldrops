package httpx

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/proxy"
)

// DialNode opens a TCP connection to addr, the host of target, through the
// proxy the transports in this package would use for target. It is for a caller
// that needs the socket and so cannot use a transport, and must not take a
// different route to the node from every other call.
func DialNode(ctx context.Context, target *url.URL, addr string) (net.Conn, error) {
	proxyURL, err := http.ProxyFromEnvironment(&http.Request{URL: target})
	if err != nil {
		return nil, fmt.Errorf("proxy for %s: %w", target.Host, err)
	}
	return dialVia(ctx, proxyURL, addr)
}

// dialVia is bounded by ctx, and proxyURL nil means a direct dial.
func dialVia(ctx context.Context, proxyURL *url.URL, addr string) (net.Conn, error) {
	direct := &net.Dialer{}
	if proxyURL == nil {
		return direct.DialContext(ctx, "tcp", addr)
	}
	switch proxyURL.Scheme {
	case "socks5", "socks5h":
		d, err := proxy.FromURL(proxyURL, direct)
		if err != nil {
			return nil, fmt.Errorf("proxy %s: %w", proxyURL.Host, err)
		}
		return d.(proxy.ContextDialer).DialContext(ctx, "tcp", addr)
	case "http":
		return dialConnect(ctx, direct, proxyURL, addr)
	}
	// Going direct here would leave the private path the operator asked for.
	return nil, fmt.Errorf("proxy scheme %q is not supported for a raw dial to %s", proxyURL.Scheme, addr)
}

func dialConnect(ctx context.Context, direct *net.Dialer, proxyURL *url.URL, addr string) (net.Conn, error) {
	proxyAddr := proxyURL.Host
	if proxyURL.Port() == "" {
		proxyAddr = net.JoinHostPort(proxyURL.Hostname(), "80")
	}
	conn, err := direct.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline) //nolint:errcheck // cleared below once the tunnel is up
	}
	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: http.Header{},
	}
	if u := proxyURL.User; u != nil {
		pass, _ := u.Password()
		req.Header.Set("Proxy-Authorization",
			"Basic "+base64.StdEncoding.EncodeToString([]byte(u.Username()+":"+pass)))
	}
	if err := req.Write(conn); err != nil {
		_ = conn.Close() //nolint:errcheck // best-effort
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		_ = conn.Close() //nolint:errcheck // best-effort
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close() //nolint:errcheck // best-effort
		_ = conn.Close()      //nolint:errcheck // best-effort
		return nil, fmt.Errorf("proxy %s refused CONNECT %s: %s", proxyURL.Host, addr, resp.Status)
	}
	_ = conn.SetDeadline(time.Time{}) //nolint:errcheck // best-effort
	return Prefixed(conn, br), nil
}
