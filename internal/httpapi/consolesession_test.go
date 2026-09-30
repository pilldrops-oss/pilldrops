package httpapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/auth/adminauth"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/process"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// The dashboard signs an account session with the console key; the API must
// take that session for the key, and nothing else signed by anyone else.
func TestConsoleSessionStandsForTheConsoleKey(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	_, consoleKey, err := st.CreateKey(t.Context(), "dashboard-session", "default")
	if err != nil {
		t.Fatal(err)
	}
	d := daemon.New(daemon.Config{
		Runtime: process.New(filepath.Join(dir, "sb")), Store: daemon.StoreAdapter{S: st},
		SnapshotDir: filepath.Join(dir, "snap"), IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	status := func(s *Server, cookie string) int {
		srv := httptest.NewServer(s.Routes())
		defer srv.Close()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/templates", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	now := time.Now()
	signed := adminauth.MintSession([]byte(consoleKey), "admin@example.com", now)
	forged := adminauth.MintSession([]byte("some-other-secret"), "admin@example.com", now)

	withKey := New(d, st)
	withKey.SetConsoleKey(consoleKey)
	if got := status(withKey, signed); got != http.StatusOK {
		t.Errorf("signed session = %d, want 200", got)
	}
	if got := status(withKey, forged); got != http.StatusUnauthorized {
		t.Errorf("session signed with another secret = %d, want 401", got)
	}
	if got := status(New(d, st), signed); got != http.StatusUnauthorized {
		t.Errorf("signed session on a node with no console key = %d, want 401", got)
	}
}
