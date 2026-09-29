package dashboard

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/runtime/process"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// newTestDash returns a running dashboard server and a valid session key.
func newTestDash(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	srv, st := newTestDashWithStore(t)
	_, key, err := st.CreateKey(t.Context(), "dash-test", "default")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	return srv, key
}

// dashGet issues a GET authenticated with the session cookie.
func dashGet(t *testing.T, base, key, path string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", base+path, nil)
	req.AddCookie(&http.Cookie{Name: "sandboxd_session", Value: key})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

// newTestDashWithStore is like newTestDash but also returns the store.
func newTestDashWithStore(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, _ := store.Open(t.Context(), filepath.Join(dir, "s.db"))
	t.Cleanup(func() { st.Close() })
	d := daemon.New(daemon.Config{
		Runtime:     process.New(filepath.Join(dir, "sb")),
		Store:       daemon.StoreAdapter{S: st},
		SnapshotDir: filepath.Join(dir, "snap"),
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	mux := http.NewServeMux()
	New(d, st, NewLogBroadcaster(), Limits{MaxActive: 100, RateRPS: 20, RateBurst: 40}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, st
}

func TestDashboardRequiresAuth(t *testing.T) {
	srv, _ := newTestDashWithStore(t)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/dashboard/api/stats"},
		{"GET", "/dashboard/api/sandboxes"},
		{"DELETE", "/dashboard/api/sandboxes/anything"},
		{"GET", "/dashboard/api/config"},
		{"PUT", "/dashboard/api/config"},
	} {
		req, _ := http.NewRequest(tc.method, srv.URL+tc.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s without auth = %d, want 401", tc.method, tc.path, resp.StatusCode)
		}
	}
}

func TestDashboardAcceptsSessionCookieAndBearer(t *testing.T) {
	srv, st := newTestDashWithStore(t)
	_, key, err := st.CreateKey(t.Context(), "dash", "default")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	req, _ := http.NewRequest("GET", srv.URL+"/dashboard/api/stats", nil)
	req.AddCookie(&http.Cookie{Name: "sandboxd_session", Value: key})
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("with session cookie = %v status=%d, want 200", err, resp.StatusCode)
	}
	resp.Body.Close()

	req, _ = http.NewRequest("GET", srv.URL+"/dashboard/api/stats", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("with bearer = %v status=%d, want 200", err, resp.StatusCode)
	}
	resp.Body.Close()

	req, _ = http.NewRequest("GET", srv.URL+"/dashboard/api/stats", nil)
	req.Header.Set("Authorization", "Bearer not-a-real-key")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("bogus: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("bogus token = %d, want 401", resp.StatusCode)
	}
}

// Otherwise http.Server.Shutdown blocks on the streams until its drain timeout.
func TestSSEStreamsEndOnClose(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.Open(t.Context(), filepath.Join(dir, "s.db"))
	t.Cleanup(func() { st.Close() })
	d := daemon.New(daemon.Config{
		Runtime:     process.New(filepath.Join(dir, "sb")),
		Store:       daemon.StoreAdapter{S: st},
		SnapshotDir: filepath.Join(dir, "snap"),
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	dash := New(d, st, NewLogBroadcaster(), Limits{MaxActive: 100, RateRPS: 20, RateBurst: 40})
	mux := http.NewServeMux()
	dash.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	_, key, err := st.CreateKey(t.Context(), "sse", "default")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	done := make(chan string, 2)
	for _, path := range []string{"/dashboard/api/events", "/dashboard/api/logs"} {
		go func(p string) {
			req, _ := http.NewRequest("GET", srv.URL+p, nil)
			req.AddCookie(&http.Cookie{Name: "sandboxd_session", Value: key})
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				done <- p + ": " + err.Error()
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body) // returns only when the stream ends
			resp.Body.Close()
			done <- p
		}(path)
	}
	time.Sleep(200 * time.Millisecond) // let both streams connect

	dash.Close()

	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("SSE stream still open 3s after Close — Shutdown would block on it")
		}
	}
}

func TestDashboardDataEndpoints(t *testing.T) {
	srv, key := newTestDash(t)
	for _, path := range []string{"/dashboard/api/stats", "/dashboard/api/sandboxes", "/dashboard/api/config"} {
		resp := dashGet(t, srv.URL, key, path)
		if resp.StatusCode != 200 {
			t.Fatalf("%s -> %d", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestStatsReportsLimits(t *testing.T) {
	srv, key := newTestDash(t)
	resp := dashGet(t, srv.URL, key, "/dashboard/api/stats")
	if resp.StatusCode != 200 {
		t.Fatalf("GET /dashboard/api/stats: status=%v", resp.StatusCode)
	}
	defer resp.Body.Close()

	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	if got["max_active"].(float64) != 100 {
		t.Errorf("max_active: want 100, got %v", got["max_active"])
	}
	if got["rate_limit_rps"].(float64) != 20 {
		t.Errorf("rate_limit_rps: want 20, got %v", got["rate_limit_rps"])
	}
	if got["rate_limit_burst"].(float64) != 40 {
		t.Errorf("rate_limit_burst: want 40, got %v", got["rate_limit_burst"])
	}
	if got["max_concurrent"].(float64) != 100 {
		t.Errorf("max_concurrent: want 100 (alias of MaxActive), got %v", got["max_concurrent"])
	}
}
