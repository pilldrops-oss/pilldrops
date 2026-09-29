package proxy

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// touchResolver counts every idle-timer reset: the one PreviewTarget does on
// resolve plus each ticker Touch from a held websocket.
type touchResolver struct {
	stubResolver
	touches atomic.Int32
}

func (t *touchResolver) PreviewTarget(ctx context.Context, id string, port int, org string) (string, error) {
	t.touches.Add(1)
	return t.stubResolver.PreviewTarget(ctx, id, port, org)
}

func (t *touchResolver) Touch(string) { t.touches.Add(1) }

// upgradeUpstream answers a websocket upgrade with 101 and holds the hijacked
// connection open until the client closes it.
func upgradeUpstream(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			io.WriteString(w, "plain")
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("upstream hijack: %v", err)
			return
		}
		defer conn.Close()
		io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		io.Copy(io.Discard, conn) // returns when the client hangs up
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func dialUpgrade(t *testing.T, proxyURL, path string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(proxyURL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(conn, "GET "+path+" HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade = %d, want 101", resp.StatusCode)
	}
	return conn
}

func gated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(WithPreviewOrg(r.Context(), "u:cust1")))
	})
}

func TestHeldWebsocketKeepsTouchingTheSandbox(t *testing.T) {
	old := touchInterval
	touchInterval = 10 * time.Millisecond
	t.Cleanup(func() { touchInterval = old })

	res := &touchResolver{stubResolver: stubResolver{target: upgradeUpstream(t)}}
	proxy := httptest.NewServer(gated(Handler(res)))
	defer proxy.Close()

	conn := dialUpgrade(t, proxy.URL, "/preview/sb_1/8080/websockify")
	time.Sleep(100 * time.Millisecond)
	held := res.touches.Load()
	if held < 4 {
		t.Fatalf("touches while the socket was held = %d, want >= 4 (1 resolve + ticker) — "+
			"an open viewer would be paused under the idle timer", held)
	}

	conn.Close()
	time.Sleep(50 * time.Millisecond)
	after := res.touches.Load()
	time.Sleep(50 * time.Millisecond)
	if got := res.touches.Load(); got != after {
		t.Fatalf("ticker still running after close: %d -> %d", after, got)
	}
}

func TestPlainRequestTouchesOnce(t *testing.T) {
	old := touchInterval
	touchInterval = 10 * time.Millisecond
	t.Cleanup(func() { touchInterval = old })

	res := &touchResolver{stubResolver: stubResolver{target: upgradeUpstream(t)}}
	proxy := httptest.NewServer(gated(Handler(res)))
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/preview/sb_1/8080/")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	time.Sleep(50 * time.Millisecond)
	if got := res.touches.Load(); got != 1 {
		t.Fatalf("touches for a plain GET = %d, want exactly 1 (the resolve)", got)
	}
}

// The subdomain form forwards through HostRouter, not Handler; it must keep
// the viewer awake too.
func TestHostFormWebsocketKeepsTouching(t *testing.T) {
	old := touchInterval
	touchInterval = 10 * time.Millisecond
	t.Cleanup(func() { touchInterval = old })

	res := &touchResolver{stubResolver: stubResolver{target: upgradeUpstream(t)}}
	verify := func(id string, port int, tok string) (string, bool) { return "u:cust1", true }
	proxy := httptest.NewServer(HostRouter("sb.test", res, http.NotFoundHandler(), verify))
	defer proxy.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(proxy.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "GET /websockify?ptok=x HTTP/1.1\r\nHost: 8080-sb-1.sb.test\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade = %d, want 101", resp.StatusCode)
	}
	time.Sleep(100 * time.Millisecond)
	if held := res.touches.Load(); held < 4 {
		t.Fatalf("host-form touches = %d, want >= 4", held)
	}
}
