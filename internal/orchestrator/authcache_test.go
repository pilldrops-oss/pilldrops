package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/auth/authcache"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

func TestCustomerKeyIsValidatedOncePerCache(t *testing.T) {
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	_, plaintext, err := st.CreateKey(t.Context(), "sdk", "u:abc")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	s := NewServer("operator-key", "node-token")
	s.SetStore(st)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	call := func() int {
		req, _ := http.NewRequest("GET", srv.URL+"/v1/sandboxes", nil)
		req.Header.Set("Authorization", "Bearer "+plaintext)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if code := call(); code != 200 {
		t.Fatalf("first authenticated call: got %d, want 200", code)
	}
	if _, ok := s.authc.Get(authcache.Key(plaintext)); !ok {
		t.Fatal("a validated customer key was not cached — every request will re-query the store")
	}
	for i := 0; i < 5; i++ {
		if code := call(); code != 200 {
			t.Fatalf("cached call %d: got %d, want 200", i, code)
		}
	}
}

func TestInvalidKeyIsStillRejectedOnEveryCall(t *testing.T) {
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	s := NewServer("operator-key", "node-token")
	s.SetStore(st)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest("GET", srv.URL+"/v1/sandboxes", nil)
		req.Header.Set("Authorization", "Bearer sf_live_nope")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("call %d: got %d, want 401", i, resp.StatusCode)
		}
	}
	if _, ok := s.authc.Get(authcache.Key("sf_live_nope")); ok {
		t.Error("an invalid key was cached — a rejected credential must never become a cached one")
	}
}
