package orchestrator

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

// The forward path hand-writes its dial, so nothing else proves it takes the
// operator's proxy.
func TestForwardTunnelGoesThroughTheConfiguredProxy(t *testing.T) {
	if !reexecWithEnv(t, "PILLDROPS_TEST_TUNNEL_PROXY_CHILD") {
		return
	}

	var opened atomic.Int32
	node := fakeTunnelNode(t, &opened)
	nodeURL, err := url.Parse(node.URL)
	if err != nil {
		t.Fatal(err)
	}
	// A name only the proxy can resolve, and loopback is never proxied anyway.
	throughProxy := "node.invalid:" + nodeURL.Port()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	asked := make(chan string, 1)
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
		asked <- req.Host
		up, err := net.Dial("tcp", nodeURL.Host)
		if err != nil {
			return
		}
		defer up.Close()
		_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\n\r\n")
		go func() { _, _ = io.Copy(up, c) }()
		_, _ = io.Copy(c, up)
	}()
	t.Setenv("HTTP_PROXY", "http://"+l.Addr().String())

	s := NewServer("fleet-key", "nodetoken")
	if err := s.reg.Upsert(cluster.NodeUpsert{ID: "n1", URL: "http://" + throughProxy, Key: "kk"}, s.now()); err != nil {
		t.Fatal(err)
	}
	s.reg.Place("sb1", "n1")

	conn, err := s.dialNodeTunnel(t.Context(), "sb1", 5432, "tcp")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if got := <-asked; got != throughProxy {
		t.Errorf("proxy was asked for %q, want %q", got, throughProxy)
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, "ping"); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "ping" || opened.Load() != 1 {
		t.Fatalf("echoed %q over %d tunnels", got, opened.Load())
	}
}
