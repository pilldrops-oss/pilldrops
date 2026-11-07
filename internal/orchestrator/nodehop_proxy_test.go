package orchestrator

import (
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

// reexecWithEnv runs the calling test's body in a child process and reports
// whether this process is that child. http.ProxyFromEnvironment reads the
// environment once per process, so the body has to be its first caller.
func reexecWithEnv(t *testing.T, childEnv string) (isChild bool) {
	t.Helper()
	if os.Getenv(childEnv) != "" {
		return true
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v")
	cmd.Env = append(os.Environ(), childEnv+"=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return false
}

// socks5ToLoopback is a no-auth SOCKS5 proxy that serves domain-name targets by
// dialling the same port on loopback, and reports each target it was asked for.
func socks5ToLoopback(t *testing.T, asked chan<- string) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	serve := func(c net.Conn) {
		defer c.Close()
		hello := make([]byte, 2)
		if _, err := io.ReadFull(c, hello); err != nil {
			return
		}
		if _, err := io.CopyN(io.Discard, c, int64(hello[1])); err != nil {
			return
		}
		_, _ = c.Write([]byte{5, 0})
		head := make([]byte, 5) // version, command, reserved, address type, name length
		if _, err := io.ReadFull(c, head); err != nil || head[3] != 3 {
			return
		}
		rest := make([]byte, int(head[4])+2)
		if _, err := io.ReadFull(c, rest); err != nil {
			return
		}
		port := strconv.Itoa(int(binary.BigEndian.Uint16(rest[len(rest)-2:])))
		asked <- net.JoinHostPort(string(rest[:len(rest)-2]), port)
		up, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", port))
		if err != nil {
			return
		}
		defer up.Close()
		_, _ = c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
		go func() { _, _ = io.Copy(up, c) }()
		_, _ = io.Copy(c, up)
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go serve(c)
		}
	}()
	return l.Addr().String()
}

// Every transport on the hop, for a pinned node, through a SOCKS5 proxy: the
// route a userspace mesh client offers. The name resolves only at the proxy, so
// a client that went direct could not have connected at all.
func TestEveryHopModeReachesAPinnedNodeThroughSOCKS5(t *testing.T) {
	if !reexecWithEnv(t, "PILLDROPS_TEST_HOP_SOCKS_CHILD") {
		return
	}
	id, srv, sawTLS := tlsNode(t)
	nodeURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	want := "node.invalid:" + nodeURL.Port()
	asked := make(chan string, 64)
	t.Setenv("HTTPS_PROXY", "socks5://"+socks5ToLoopback(t, asked))

	s := hopServer()
	node := &cluster.Node{ID: "n1", URL: "http://" + want, Key: "k", TLSSPKI: id.SPKI}
	for name, mode := range map[string]hopMode{"buffered": hopBuffered, "stream": hopStream, "work": hopWork} {
		base, client, err := s.nodeHop(node, mode)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.HasPrefix(base, "https://") {
			t.Fatalf("%s: base %q did not take the pin's upgrade", name, base)
		}
		resp, err := client.Get(base + "/v1/sandboxes")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d", name, resp.StatusCode)
		}
		if got := <-asked; got != want {
			t.Errorf("%s: proxy was asked for %q, want %q", name, got, want)
		}
	}
	if len(*sawTLS) != 3 {
		t.Fatalf("node saw %d requests, want 3", len(*sawTLS))
	}
	for i, encrypted := range *sawTLS {
		if !encrypted {
			t.Errorf("request %d reached the node unencrypted", i)
		}
	}
}
