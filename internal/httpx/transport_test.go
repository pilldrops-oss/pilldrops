package httpx

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestTransportsHaveADialTimeout(t *testing.T) {
	for name, tr := range map[string]*http.Transport{
		"NewTransport":       NewTransport(),
		"NewFanoutTransport": NewFanoutTransport(),
	} {
		if tr.DialContext == nil {
			t.Fatalf("%s: DialContext is nil — that means NO dial timeout, which is the exact defect this package fixes", name)
		}
		// A deadline well under DialTimeout must win, proving ctx reaches the dialer.
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		start := time.Now()
		_, err := tr.DialContext(ctx, "tcp", "203.0.113.1:9")
		cancel()
		if err == nil {
			t.Fatalf("%s: unexpectedly connected to a TEST-NET address", name)
		}
		if elapsed := time.Since(start); elapsed > DialTimeout {
			t.Errorf("%s: a 150ms context did not cut the dial short (took %v) — the context "+
				"is not reaching the dialer", name, elapsed)
		}

		// No caller deadline; TEST-NET-3 hangs until something times it out.
		// The one-minute bound is loose because a tight margin flakes under CI
		// load; the 90s guard only stops a broken transport hanging the package.
		guard, cancelGuard := context.WithTimeout(context.Background(), 90*time.Second)
		start = time.Now()
		_, err = tr.DialContext(guard, "tcp", "203.0.113.1:9")
		cancelGuard()
		elapsed := time.Since(start)
		if err == nil {
			t.Fatalf("%s: unexpectedly connected to a TEST-NET address", name)
		}
		if elapsed > time.Minute {
			t.Errorf("%s: dial took %v — that is the OS timing out, not this package "+
				"(DialTimeout is %v)", name, elapsed, DialTimeout)
		}
	}
}

func TestTransportsPoolMoreThanTheDefaultPerHost(t *testing.T) {
	if got := NewTransport().MaxIdleConnsPerHost; got <= http.DefaultMaxIdleConnsPerHost {
		t.Errorf("MaxIdleConnsPerHost = %d, want more than the default %d",
			got, http.DefaultMaxIdleConnsPerHost)
	}
}

func TestFanoutTransportBoundsTheTotalNotThePerHostShare(t *testing.T) {
	fan, node := NewFanoutTransport(), NewTransport()
	if fan.MaxIdleConns <= 0 {
		t.Error("fanout transport must bound total idle connections")
	}
	if fan.MaxIdleConnsPerHost >= node.MaxIdleConnsPerHost {
		t.Errorf("fanout per-host (%d) should be smaller than the node transport's (%d)",
			fan.MaxIdleConnsPerHost, node.MaxIdleConnsPerHost)
	}
	if fan.MaxIdleConnsPerHost*100 < fan.MaxIdleConns {
		t.Log("note: per-host * 100 is below the total cap, so the total is the binding constraint — intended")
	}
}

func TestTransportsBoundAStalledResponse(t *testing.T) {
	for name, tr := range map[string]*http.Transport{
		"NewTransport":       NewTransport(),
		"NewFanoutTransport": NewFanoutTransport(),
	} {
		if tr.ResponseHeaderTimeout <= 0 {
			t.Errorf("%s: ResponseHeaderTimeout unset — a peer that accepts and stays silent ties up a goroutine", name)
		}
		if tr.IdleConnTimeout <= 0 {
			t.Errorf("%s: IdleConnTimeout unset — idle connections accumulate", name)
		}
	}
}

func TestFanoutTransportDoesNotAttemptHTTP2(t *testing.T) {
	if NewFanoutTransport().ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 on the sandbox-facing transport breaks WebSocket upgrades")
	}
}

func TestConstructorsReturnIndependentTransports(t *testing.T) {
	a, b := NewTransport(), NewTransport()
	if a == b {
		t.Error("NewTransport returned the same instance twice")
	}
	a.MaxIdleConns = 1
	if b.MaxIdleConns == 1 {
		t.Error("transports share state")
	}
}

func TestTransportServesRealRequests(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	c := &http.Client{Transport: NewTransport(), Timeout: 5 * time.Second}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+ln.Addr().String()+"/", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
}
