package orchestrator

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// fakeTunnelNode answers the node's /tcp/ route the way internal/httpapi does:
// hijack, 101, then echo. Records how many tunnels were opened.
func fakeTunnelNode(t *testing.T, opened *atomic.Int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sandboxes/{id}/tcp/{port}", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ptok") == "" {
			http.Error(w, "no token", http.StatusNotFound)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		opened.Add(1) // the handler runs on the server's goroutine; the test reads it
		_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\n\r\n")
		defer conn.Close()
		_, _ = io.Copy(conn, conn)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// The ingress binds what the store says, relays bytes through the node, and
// unbinds when the row goes away. Everything a second replica's INSERT would
// do is done by writing the row directly, which is what the ingress sees.
func TestForwardIngressBindsRelaysAndUnbinds(t *testing.T) {
	t.Setenv("FORWARD_INGRESS", "1")
	t.Setenv("FORWARD_PUBLIC_HOST", "fwd.example.net")

	var opened atomic.Int32
	node := fakeTunnelNode(t, &opened)

	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "ingress.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	s := NewServer("fleet-key", "nodetoken")
	s.SetStore(st)
	if err := s.reg.Upsert(cluster.NodeUpsert{ID: "n1", URL: node.URL, Key: "kk"}, s.now()); err != nil {
		t.Fatal(err)
	}
	s.reg.SetNodeMeta("n1", cluster.NodeMeta{Runtime: "firecracker", Isolation: true})
	if err := st.SaveSandbox(t.Context(), store.SandboxRecord{
		ID: "sb1", OrgID: "u:one", Template: "base", State: "running",
		NodeID: "n1", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	s.reg.Place("sb1", "n1")

	// A free public port, so the range cannot collide with anything on the box.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()
	t.Setenv("FORWARD_PORT_RANGE", strconv.Itoa(port)+"-"+strconv.Itoa(port))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.StartForwardIngress(ctx)

	row, err := st.UpsertPortForward(t.Context(),
		store.PortForward{SandboxID: "sb1", GuestPort: 5432, TCP: true, CreatedAt: 1}, port, port)
	if err != nil {
		t.Fatal(err)
	}
	if row.PublicPort != port {
		t.Fatalf("allocated %d, want the only port in the range %d", row.PublicPort, port)
	}
	s.reconcileForwards(t.Context())

	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 5*time.Second)
	if err != nil {
		t.Fatalf("the ingress did not bind %d: %v", port, err)
	}
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	payload := make([]byte, 256<<10)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = conn.Write(payload) }()
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read back through the public port: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("bytes through the public port came back altered")
	}
	_ = conn.Close()
	if n := opened.Load(); n != 1 {
		t.Fatalf("the node saw %d tunnels, want 1", n)
	}

	// Removing the row unbinds the port, and an open stream on it is closed.
	live, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := live.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, 4)
	_ = live.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(live, echo); err != nil || string(echo) != "ping" {
		t.Fatalf("stream not live before the delete: %q %v", echo, err)
	}
	if ok, err := st.DeletePortForward(t.Context(), "sb1", 5432); err != nil || !ok {
		t.Fatalf("delete: %v %v", ok, err)
	}
	s.reconcileForwards(t.Context())

	// The read must END, not merely fail to produce bytes: a deadline expiring
	// is exactly what an untouched, idle stream does, so accepting any error
	// here would pass against an ingress that never closes anything.
	_ = live.SetDeadline(time.Now().Add(5 * time.Second))
	_, err = live.Read(make([]byte, 16))
	switch {
	case err == nil:
		t.Fatal("an open stream still carried bytes after the row was removed")
	case os.IsTimeout(err):
		t.Fatal("an open stream was left hanging after the row was removed; " +
			"it must see EOF, and a read that merely times out proves nothing")
	}
	if c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 2*time.Second); err == nil {
		_ = c.Close()
		t.Fatalf("port %d still accepts connections after the row was removed", port)
	}
}

// A replica without FORWARD_INGRESS binds nothing, which is what lets the API
// run on every replica while exactly one host terminates the raw ports.
func TestForwardIngressIsOptIn(t *testing.T) {
	t.Setenv("FORWARD_INGRESS", "")
	s := NewServer("k", "t")
	s.StartForwardIngress(t.Context())
	if s.ingress.Load() != nil {
		t.Fatal("an ingress started without FORWARD_INGRESS=1")
	}
	// Positive control: it does start when the flag is set.
	t.Setenv("FORWARD_INGRESS", "1")
	s2 := NewServer("k", "t")
	s2.StartForwardIngress(t.Context())
	if s2.ingress.Load() == nil {
		t.Fatal("FORWARD_INGRESS=1 did not start the ingress")
	}
}
