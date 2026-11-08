package httpapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/runtime/runtimetest"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// reservedSlugServer is serviceTemplateServer that also returns the daemon, so
// a test can give a sandbox a slug the API now refuses, the way a sandbox that
// held it before it was reserved has it.
func reservedSlugServer(t *testing.T) (*httptest.Server, *daemon.Daemon, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	_, key, err := st.CreateKey(t.Context(), "test", "default")
	if err != nil {
		t.Fatal(err)
	}
	d := daemon.New(daemon.Config{
		Runtime:     runtimetest.NewFake(),
		Store:       daemon.StoreAdapter{S: st},
		SnapshotDir: filepath.Join(dir, "snap"),
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	s := New(d, st)
	s.SetOrgOverrideSecret([]byte(testOrgOverrideSecret))
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	return srv, d, key
}

func TestNewlyReservedSlugsAreRefusedButKeptByTheirHolder(t *testing.T) {
	srv, d, key := reservedSlugServer(t)

	if code, out := createJSON(t, srv, key, `{"template":"base","slug":"login"}`); code != http.StatusBadRequest {
		t.Fatalf("create with slug login = %d %v, want 400", code, out)
	}

	code, held := createJSON(t, srv, key, `{"template":"base"}`)
	if code != http.StatusCreated {
		t.Fatalf("create = %d %v", code, held)
	}
	heldID, _ := held["id"].(string)
	if err := d.SetSlug(t.Context(), heldID, "docs"); err != nil {
		t.Fatalf("give the holder its old slug: %v", err)
	}
	// The holder re-sending its own slug beside another change is not a new claim.
	if code, out := patchSandbox(t, srv, key, heldID, `{"slug":"docs","name":"handbook"}`); code != http.StatusOK || out["slug"] != "docs" {
		t.Fatalf("holder PATCH = %d %v, want 200 keeping slug docs", code, out)
	}

	code, other := createJSON(t, srv, key, `{"template":"base"}`)
	if code != http.StatusCreated {
		t.Fatalf("create = %d %v", code, other)
	}
	otherID, _ := other["id"].(string)
	if code, out := patchSandbox(t, srv, key, otherID, `{"slug":"status"}`); code != http.StatusBadRequest {
		t.Fatalf("PATCH a fresh sandbox onto slug status = %d %v, want 400", code, out)
	}
}
