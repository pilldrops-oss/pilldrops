package orchestrator

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

func TestReplaySafe(t *testing.T) {
	for _, tc := range []struct {
		method string
		want   bool
	}{
		{http.MethodGet, true},
		{http.MethodHead, true},
		{http.MethodPut, true},
		{http.MethodDelete, true},
		{http.MethodPost, false},
		{http.MethodPatch, false},
	} {
		if got := replaySafe(tc.method); got != tc.want {
			t.Errorf("replaySafe(%s) = %v, want %v", tc.method, got, tc.want)
		}
	}
}

func TestNotDeliveredOnlyTrueForDialFailures(t *testing.T) {
	dialErr := &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	if !notDelivered(dialErr) {
		t.Error("a dial failure never put bytes on the wire — want notDelivered=true")
	}
	if !notDelivered(&net.OpError{Op: "dial", Err: errors.New("no route to host")}) {
		t.Error("want notDelivered=true for a wrapped dial failure")
	}
	for name, err := range map[string]error{
		"timeout":       errors.New("Client.Timeout exceeded while awaiting headers"),
		"read reset":    &net.OpError{Op: "read", Err: errors.New("connection reset by peer")},
		"write failure": &net.OpError{Op: "write", Err: errors.New("broken pipe")},
	} {
		if notDelivered(err) {
			t.Errorf("%s is ambiguous — the node may have executed the request; want notDelivered=false", name)
		}
	}
}

func TestForwardDoesNotReplayPostOnAmbiguousError(t *testing.T) {
	var hits int32
	// Hijack-and-close: connected, so not a dial failure, then dropped without a response.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("test server cannot hijack")
			return
		}
		c, _, err := hj.Hijack()
		if err != nil {
			return
		}
		_ = c.Close()
	}))
	defer srv.Close()

	s := &Server{client: srv.Client()}
	node := &cluster.Node{ID: "n1", URL: srv.URL}

	if _, err := s.forward(t.Context(), node, http.MethodPost, "/v1/sandboxes", []byte(`{}`), "application/json"); err == nil {
		t.Fatal("want an error from the dropped connection")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("node saw %d POSTs, want exactly 1 — a replayed create duplicates the sandbox", got)
	}
}

func TestForwardStillRetriesIdempotentMethods(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) < 3 {
			hj, _, err := hijack(w)
			if err == nil {
				_ = hj.Close()
			}
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := &Server{client: srv.Client()}
	node := &cluster.Node{ID: "n1", URL: srv.URL}

	resp, err := s.forward(t.Context(), node, http.MethodGet, "/v1/sandboxes", nil, "")
	if err != nil {
		t.Fatalf("GET should have retried to success: %v", err)
	}
	defer resp.Body.Close()
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("node saw %d GETs, want 3 (1 + 2 retries)", got)
	}
}

func TestForwardReplaysPostWhenNothingWasDelivered(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + ln.Addr().String()
	_ = ln.Close() // every attempt now fails at dial

	var attempts int32
	s := &Server{
		client: &http.Client{Transport: countingTransport{n: &attempts, base: http.DefaultTransport}},
	}
	node := &cluster.Node{ID: "n1", URL: dead}

	if _, err := s.forward(t.Context(), node, http.MethodPost, "/v1/sandboxes", []byte(`{}`), "application/json"); err == nil {
		t.Fatal("want an error against a dead node")
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("attempts = %d, want 3 — a dial failure delivered nothing, so replay is safe", got)
	}
}

type countingTransport struct {
	n    *int32
	base http.RoundTripper
}

func (c countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	atomic.AddInt32(c.n, 1)
	return c.base.RoundTrip(r)
}

func hijack(w http.ResponseWriter) (net.Conn, any, error) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("not hijackable")
	}
	c, _, err := hj.Hijack()
	return c, nil, err
}
