package httpapi

import (
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/process"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// desktopCapableRuntime is the process runtime claiming it can host an X
// server, so the two halves of has_desktop can be varied independently. Only
// Capabilities is overridden; everything else really boots.
type desktopCapableRuntime struct {
	rt.Runtime
	desktop bool
}

func (r desktopCapableRuntime) Capabilities() rt.Caps {
	c := r.Runtime.Capabilities()
	c.Desktop = r.desktop
	return c
}

// newDesktopServer is newTestServerWithStore with the runtime's Desktop bit
// under the test's control.
func newDesktopServer(t *testing.T, desktop bool) (*httptest.Server, *Server, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	_, key, err := st.CreateKey(t.Context(), "test", "default")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	d := daemon.New(daemon.Config{
		Runtime:     desktopCapableRuntime{Runtime: process.New(filepath.Join(dir, "sb")), desktop: desktop},
		Store:       daemon.StoreAdapter{S: st},
		SnapshotDir: filepath.Join(dir, "snap"),
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	s := New(d, st)
	attachTestFunctions(s, d, st, dir)
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	return srv, s, key
}

// has_desktop is the AND of three things, and no subset may decide it: the
// runtime can host an X server, the image holds the screen stack, and this
// sandbox asked for it.
//
// The third clause is the one that was missing, and these rows fail against the
// old code: `base` claimed a desktop nobody had asked for, envd started no
// screen on it, and `sandbox open` handed out a link to port 6080 that nothing
// was ever listening on.
func TestHasDesktopCombinesRuntimeImageAndRequest(t *testing.T) {
	for _, tc := range []struct {
		name     string
		desktop  bool
		template string
		want     bool
	}{
		{"platform template that starts no screen", true, "base", false},
		{"the desktop template bakes the flag", true, "desktop", true},
		{"alias of a platform template that starts no screen", true, "python-3.11", false},
		{"service template runs a vendor image", true, "postgres", false},
		{"caller-supplied image", true, "ghcr.io/someone/thing:1", false},
		{"runtime cannot host X", false, "desktop", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, s, _ := newDesktopServer(t, tc.desktop)
			sb := daemon.SandboxInfo{Template: tc.template}
			if got := s.hasDesktop(sb); got != tc.want {
				t.Errorf("hasDesktop(%q) with runtime desktop=%v = %v, want %v",
					tc.template, tc.desktop, got, tc.want)
			}
		})
	}
}

// Absent is not false to a client, so the field has to be on the wire from
// every response that describes a sandbox.
func TestHasDesktopOnCreateGetAndList(t *testing.T) {
	srv, _, key := newDesktopServer(t, true)

	code, created := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"desktop"}`)
	if code != 201 {
		t.Fatalf("create = %d %v", code, created)
	}
	if created["has_desktop"] != true {
		t.Errorf("create: has_desktop = %v, want true", created["has_desktop"])
	}
	id, _ := created["id"].(string)

	code, got := do(t, "GET", srv.URL+"/v1/sandboxes/"+id, key, "")
	if code != 200 {
		t.Fatalf("get = %d %v", code, got)
	}
	if got["has_desktop"] != true {
		t.Errorf("get: has_desktop = %v, want true", got["has_desktop"])
	}

	code, listed := do(t, "GET", srv.URL+"/v1/sandboxes", key, "")
	if code != 200 {
		t.Fatalf("list = %d %v", code, listed)
	}
	rows, _ := listed["sandboxes"].([]any)
	if len(rows) != 1 {
		t.Fatalf("list returned %d sandboxes, want 1: %v", len(rows), listed)
	}
	row, _ := rows[0].(map[string]any)
	if row["has_desktop"] != true {
		t.Errorf("list: has_desktop = %v, want true", row["has_desktop"])
	}
	// A sandbox that really does bring a screen up still gets its link.
	for _, resp := range []map[string]any{created, got} {
		u, _ := resp["desktop_url"].(string)
		if !strings.Contains(u, "/"+strconv.Itoa(rt.DesktopPort)+"/") {
			t.Errorf("desktop_url = %q, want a link on port %d", u, rt.DesktopPort)
		}
		if !strings.Contains(u, id) {
			t.Errorf("desktop_url = %q, want it to name sandbox %s", u, id)
		}
	}

	// The decoration must not eat the fields the list already carried.
	for _, k := range []string{"id", "template", "state", "created_at"} {
		if _, ok := row[k]; !ok {
			t.Errorf("list: decorating dropped %q from the row", k)
		}
	}
}

// The link that made `sandbox open` useless: a base sandbox was handed a 6080
// URL, the first link a client picks up, and nothing ever listened there.
func TestNoDesktopURLWhereNoDesktopComesUp(t *testing.T) {
	srv, _, key := newDesktopServer(t, true)

	code, created := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base"}`)
	if code != 201 {
		t.Fatalf("create = %d %v", code, created)
	}
	id, _ := created["id"].(string)
	_, got := do(t, "GET", srv.URL+"/v1/sandboxes/"+id, key, "")

	for what, resp := range map[string]map[string]any{"create": created, "get": got} {
		if resp["has_desktop"] != false {
			t.Errorf("%s: has_desktop = %v on a base sandbox nobody asked a screen of",
				what, resp["has_desktop"])
		}
		if u, present := resp["desktop_url"]; present {
			t.Errorf("%s: desktop_url = %v, and nothing is listening on %d — a link that "+
				"cannot open is worse than no link", what, u, rt.DesktopPort)
		}
	}
}
