package nodeboot

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/nodetls"
)

func TestOnePortServesTLSAndPlaintext(t *testing.T) {
	id, srvURL, plainURL, stop := serveMux(t)
	defer stop()

	// Plaintext must keep working: browsers hitting previews on this port refuse a self-signed cert.
	if body := mustGet(t, plainURL, nil); body != "hello" {
		t.Errorf("plaintext body = %q, want hello", body)
	}

	cfg, err := nodetls.PinnedClientConfig(id.SPKI)
	if err != nil {
		t.Fatal(err)
	}
	if body := mustGet(t, srvURL, cfg); body != "hello" {
		t.Errorf("TLS body = %q, want hello", body)
	}

	// The served key must be the advertised key.
	other := mustOtherIdentity(t)
	wrong, err := nodetls.PinnedClientConfig(other.SPKI)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := get(srvURL, wrong); err == nil {
		t.Error("a client pinned to a different key completed a TLS handshake with this node")
	}
}

func TestASilentClientDoesNotBlockOtherConnections(t *testing.T) {
	_, _, plainURL, stop := serveMux(t)
	defer stop()

	host := plainURL[len("http://"):]
	silent, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close() //nolint:errcheck // test cleanup

	// A real request must complete well inside sniffTimeout while the silent connection is open.
	done := make(chan string, 1)
	go func() { done <- mustGet(t, plainURL, nil) }()
	select {
	case body := <-done:
		if body != "hello" {
			t.Errorf("body = %q", body)
		}
	case <-time.After(sniffTimeout / 2):
		t.Fatal("a request was blocked by an unrelated silent connection — the sniff is serializing accepts")
	}
}

func TestMixedProtocolsUnderConcurrency(t *testing.T) {
	id, srvURL, plainURL, stop := serveMux(t)
	defer stop()
	cfg, err := nodetls.PinnedClientConfig(id.SPKI)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := range 20 {
		wg.Add(2)
		go func() { defer wg.Done(); _, e := get(plainURL, nil); errs <- e }()
		go func() { defer wg.Done(); _, e := get(srvURL, cfg); errs <- e }()
		_ = i
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Errorf("request failed: %v", e)
		}
	}
}

// serveMux starts an http.Server behind the protocol mux on a free port.
func serveMux(t *testing.T) (id *nodetls.Identity, httpsURL, httpURL string, stop func()) {
	t.Helper()
	dir := t.TempDir()
	id, err := nodetls.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := newProtocolMux(ln, id.ServerConfig())
	srv := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "hello") }),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(mux) }()
	addr := ln.Addr().String()
	return id, "https://" + addr, "http://" + addr, func() {
		_ = srv.Close()
		_ = mux.Close()
	}
}

func mustOtherIdentity(t *testing.T) *nodetls.Identity {
	t.Helper()
	other, err := nodetls.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return other
}

func mustGet(t *testing.T, url string, cfg *tls.Config) string {
	t.Helper()
	body, err := get(url, cfg)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return body
}

func get(url string, cfg *tls.Config) (string, error) {
	tr := &http.Transport{TLSClientConfig: cfg}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Timeout: 5 * time.Second, Transport: tr}).Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return string(b), nil
}
