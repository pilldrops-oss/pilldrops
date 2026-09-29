package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/auth/previewtoken"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/runtimetest"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// echoListener answers every connection by copying bytes back until EOF.
func echoListener(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// forwardNode is a node server on a real TCP listener (the tunnel hijacks, so
// httptest's in-process handler is not enough) with one sandbox on the fake
// runtime, whose ListenTarget resolves 127.0.0.1:<port>.
func forwardNode(t *testing.T) (addr, id, org string, d *daemon.Daemon) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fake := runtimetest.NewFake()
	fake.MultiPort = true
	d = daemon.New(daemon.Config{
		Runtime: fake, Store: daemon.StoreAdapter{S: st},
		SnapshotDir: filepath.Join(dir, "snap"), IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	s := New(d, st)
	s.SetPreviewSecret([]byte(testPreviewSecret))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: s.Routes(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	org = "u:cust1"
	sb, err := d.Create(context.Background(), daemon.CreateSpec{Template: "base", OrgID: org})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return ln.Addr().String(), sb.ID, org, d
}

// openTunnel performs the hand-rolled upgrade the ingress performs. Returns the
// raw stream on 101, or the status line for anything else.
func openTunnel(t *testing.T, addr, id string, port int, tok string) (net.Conn, int) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	req := &http.Request{
		Method: http.MethodGet,
		URL:    &url.URL{Path: "/v1/sandboxes/" + id + "/tcp/" + strconv.Itoa(port), RawQuery: "ptok=" + url.QueryEscape(tok)},
		Proto:  "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Host:   addr,
		Header: http.Header{"Connection": {"Upgrade"}, "Upgrade": {"pilldrops-l4"}},
	}
	if err := req.Write(c); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = c.Close()
		return nil, resp.StatusCode
	}
	t.Cleanup(func() { _ = c.Close() })
	return prefixed{Conn: c, r: br}, resp.StatusCode
}

type prefixed struct {
	net.Conn
	r io.Reader
}

func (p prefixed) Read(b []byte) (int, error) { return p.r.Read(b) }

// One MiB of random bytes must come back byte-identical: the tunnel is a splice,
// not an HTTP proxy, and anything that reframed or truncated would show here.
func TestForwardTunnelRoundTripsBytesUntouched(t *testing.T) {
	addr, id, org, _ := forwardNode(t)
	port := echoListener(t)
	tok := previewtoken.Token([]byte(testPreviewSecret), id, port, org)

	conn, code := openTunnel(t, addr, id, port, tok)
	if code != http.StatusSwitchingProtocols {
		t.Fatalf("tunnel handshake = %d, want 101", code)
	}

	payload := make([]byte, 1<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	go func() { _, _ = conn.Write(payload) }()
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("1 MiB round trip came back altered")
	}
}

// Without a valid token the route answers exactly as an unknown sandbox does,
// so it cannot be used to discover which ids or ports exist.
func TestForwardTunnelRefusesWithoutTheToken(t *testing.T) {
	addr, id, org, _ := forwardNode(t)
	port := echoListener(t)
	good := previewtoken.Token([]byte(testPreviewSecret), id, port, org)

	for _, c := range []struct {
		name, id, tok string
		port          int
	}{
		{"no token", id, "", port},
		{"junk token", id, "not-a-token", port},
		{"another port's token", id, previewtoken.Token([]byte(testPreviewSecret), id, port+1, org), port},
		{"unknown sandbox", "sb-does-not-exist", good, port},
	} {
		if _, code := openTunnel(t, addr, c.id, c.port, c.tok); code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", c.name, code)
		}
	}
	// Positive control in the same test: the real token still opens the tunnel.
	if _, code := openTunnel(t, addr, id, port, good); code != http.StatusSwitchingProtocols {
		t.Fatalf("valid token = %d, want 101 — the refusals above prove nothing if this fails too", code)
	}
}

// A guest port with nothing listening is a 502, not a hijacked connection that
// hangs: the ingress needs to close the client's connection rather than stall.
func TestForwardTunnelRefusesADeadGuestPort(t *testing.T) {
	addr, id, org, _ := forwardNode(t)
	// A port that was bound and is now free: nothing can be listening on it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	tok := previewtoken.Token([]byte(testPreviewSecret), id, dead, org)
	if _, code := openTunnel(t, addr, id, dead, tok); code != http.StatusBadGateway {
		t.Fatalf("dead guest port = %d, want 502", code)
	}
}

// A paused sandbox is woken by the connection, the same way a preview request
// wakes it; without the wake the tunnel would fail on an idle sandbox.
func TestForwardTunnelWakesAPausedSandbox(t *testing.T) {
	addr, id, org, d := forwardNode(t)
	port := echoListener(t)
	tok := previewtoken.Token([]byte(testPreviewSecret), id, port, org)

	if err := d.Pause(context.Background(), id); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if sb, _ := d.Get(id); sb.State != daemon.StatePaused {
		t.Fatalf("state after pause = %q, want paused — the wake below would prove nothing", sb.State)
	}

	conn, code := openTunnel(t, addr, id, port, tok)
	if code != http.StatusSwitchingProtocols {
		t.Fatalf("tunnel to a paused sandbox = %d, want 101", code)
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo through a woken sandbox = %q, %v", buf, err)
	}
	if sb, _ := d.Get(id); sb.State != daemon.StateRunning {
		t.Fatalf("state after the forward = %q, want running: the connection did not wake it", sb.State)
	}
}
