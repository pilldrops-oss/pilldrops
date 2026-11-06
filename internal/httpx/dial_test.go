package httpx

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func listen(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// echoServer answers one connection by echoing it, and returns its address.
func echoServer(t *testing.T) string {
	t.Helper()
	l := listen(t)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(c, c)
	}()
	return l.Addr().String()
}

func relay(a, b net.Conn) {
	go func() { _, _ = io.Copy(a, b); _ = a.Close() }()
	_, _ = io.Copy(b, a)
	_ = b.Close()
}

// fakeSOCKS5 speaks the no-auth CONNECT exchange of RFC 1928 for domain and
// IPv4 targets, and reports each target it was asked for.
func fakeSOCKS5(t *testing.T, asked chan<- string) *url.URL {
	t.Helper()
	l := listen(t)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		hello := make([]byte, 2)
		if _, err := io.ReadFull(c, hello); err != nil {
			return
		}
		if _, err := io.CopyN(io.Discard, c, int64(hello[1])); err != nil {
			return
		}
		_, _ = c.Write([]byte{5, 0})
		head := make([]byte, 4)
		if _, err := io.ReadFull(c, head); err != nil {
			return
		}
		var host string
		if head[3] == 1 {
			ip := make(net.IP, 4)
			if _, err := io.ReadFull(c, ip); err != nil {
				return
			}
			host = ip.String()
		} else {
			n := make([]byte, 1)
			if _, err := io.ReadFull(c, n); err != nil {
				return
			}
			name := make([]byte, n[0])
			if _, err := io.ReadFull(c, name); err != nil {
				return
			}
			host = string(name)
		}
		p := make([]byte, 2)
		if _, err := io.ReadFull(c, p); err != nil {
			return
		}
		port := int(p[0])<<8 | int(p[1])
		addr := net.JoinHostPort(host, strconv.Itoa(port))
		asked <- addr
		up, err := net.Dial("tcp", addr)
		if err != nil {
			_, _ = c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
			return
		}
		_, _ = c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
		relay(c, up)
	}()
	return &url.URL{Scheme: "socks5", Host: l.Addr().String()}
}

// fakeConnect is an HTTP proxy answering one CONNECT with status, and reports
// the request it received.
func fakeConnect(t *testing.T, status int, asked chan<- *http.Request) *url.URL {
	t.Helper()
	l := listen(t)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		req, err := http.ReadRequest(bufio.NewReader(c))
		if err != nil {
			return
		}
		asked <- req
		if status != http.StatusOK {
			_, _ = io.WriteString(c, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
			return
		}
		up, err := net.Dial("tcp", req.Host)
		if err != nil {
			return
		}
		_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\n\r\n")
		relay(c, up)
	}()
	return &url.URL{Scheme: "http", Host: l.Addr().String()}
}

func dialCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func assertEchoes(t *testing.T, c net.Conn) {
	t.Helper()
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, "ping"); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "ping" {
		t.Fatalf("echoed %q", got)
	}
}

func TestDialViaGoesDirectWithNoProxy(t *testing.T) {
	c, err := dialVia(dialCtx(t), nil, echoServer(t))
	if err != nil {
		t.Fatal(err)
	}
	assertEchoes(t, c)
}

func TestDialViaSOCKS5(t *testing.T) {
	target := echoServer(t)
	asked := make(chan string, 1)
	c, err := dialVia(dialCtx(t), fakeSOCKS5(t, asked), target)
	if err != nil {
		t.Fatal(err)
	}
	if got := <-asked; got != target {
		t.Errorf("proxy was asked for %q, want %q", got, target)
	}
	assertEchoes(t, c)
}

func TestDialViaHTTPConnect(t *testing.T) {
	target := echoServer(t)
	asked := make(chan *http.Request, 1)
	proxyURL := fakeConnect(t, http.StatusOK, asked)
	proxyURL.User = url.UserPassword("op", "secret")
	c, err := dialVia(dialCtx(t), proxyURL, target)
	if err != nil {
		t.Fatal(err)
	}
	req := <-asked
	if req.Method != http.MethodConnect || req.Host != target {
		t.Errorf("proxy got %s %s, want CONNECT %s", req.Method, req.Host, target)
	}
	if req.Header.Get("Proxy-Authorization") == "" {
		t.Error("the proxy URL's credentials were not sent")
	}
	assertEchoes(t, c)
}

func TestDialViaReportsAConnectRefusal(t *testing.T) {
	asked := make(chan *http.Request, 1)
	_, err := dialVia(dialCtx(t), fakeConnect(t, http.StatusForbidden, asked), "192.0.2.1:9")
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("want an error naming the 403, got %v", err)
	}
}

// A proxy this cannot speak must not become a direct dial: the operator set it
// to keep this traffic on a private path.
func TestDialViaRefusesAnUnsupportedProxyRatherThanGoingDirect(t *testing.T) {
	target := echoServer(t)
	_, err := dialVia(dialCtx(t), &url.URL{Scheme: "https", Host: "proxy.example:443"}, target)
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("want a refusal naming the scheme, got %v", err)
	}
}
