package httpapi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// useStoreTier installs a process-wide template source and dir for one test.
func useStoreTier(t *testing.T, fn templates.Source) string {
	t.Helper()
	dir := t.TempDir()
	templates.SetDir(dir)
	templates.SetSource(fn)
	t.Cleanup(func() {
		templates.SetSource(nil)
		templates.SetDir("")
	})
	return dir
}

func TestColdCreateReportsTheFetchDuration(t *testing.T) {
	useStoreTier(t, func(_ context.Context, id, dir string) error {
		time.Sleep(5 * time.Millisecond)
		if err := os.MkdirAll(filepath.Join(dir, id), 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, id, "rootfs.squashfs"), []byte("fetched"), 0o644)
	})

	srv, key := newTestServer(t)
	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base"}`)
	if code != 201 {
		t.Fatalf("a cold create returned %d (%v), want 201 — the node was placed a template "+
			"it does not hold and could not fetch it", code, out)
	}
	ms, ok := out["fetch_ms"].(float64)
	if !ok {
		t.Fatalf("the create response carries no fetch_ms: %v", out)
	}
	if ms < 1 {
		t.Errorf("fetch_ms = %v; the source slept 5ms, so this is not measuring the fetch", ms)
	}

	// The field's presence is the signal, so a warm create must omit it.
	_, warm := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base"}`)
	if _, present := warm["fetch_ms"]; present {
		t.Errorf("a warm create reported fetch_ms = %v — every create now looks like a "+
			"download and the field stops meaning anything", warm["fetch_ms"])
	}
}

func TestUnreachableStoreIsNotABadRequest(t *testing.T) {
	useStoreTier(t, func(context.Context, string, string) error {
		return errors.New("dial tcp 10.0.0.9:443: connect: connection refused")
	})

	srv, key := newTestServer(t)
	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base"}`)
	if code == 400 {
		t.Fatalf("an unreachable artifact store answered 400 (%v) — the caller is told their "+
			"template id is wrong when it is the fleet's store that is down", out)
	}
	if code != 503 {
		t.Fatalf("an unreachable artifact store answered %d (%v), want 503", code, out)
	}
	if got, _ := out["code"].(string); got != codeTemplateFetchFailed {
		t.Errorf("error code = %q, want %q — an SDK cannot tell this apart from every other "+
			"503 the node emits, so a retry loop hides an outage", got, codeTemplateFetchFailed)
	}
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "artifact store") {
		t.Errorf("the error does not name the artifact store: %q", msg)
	}
	if strings.Contains(strings.ToLower(msg), "timeout") ||
		strings.Contains(strings.ToLower(msg), "deadline") {
		t.Errorf("the fetch failure is reported as a timeout: %q", msg)
	}
}

// templates.Resolve never fetches, so the create path's validation gate must
// not refuse a template that exists only in the store.
func TestACreateForATemplateOnlyTheStoreKnowsIsNotRefusedAsUnknown(t *testing.T) {
	const id = "t47-store-only"
	if _, ok := templates.Resolve(id); ok {
		t.Fatalf("%s resolves without the store, so this test proves nothing", id)
	}
	fetched := false
	useStoreTier(t, func(_ context.Context, want, dir string) error {
		if want != id {
			return templates.ErrNotInStore
		}
		fetched = true
		if err := os.MkdirAll(filepath.Join(dir, want), 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, want, "rootfs.squashfs"), []byte("fetched"), 0o644)
	})

	srv, key := newTestServer(t)
	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"`+id+`"}`)
	if code != 201 {
		t.Fatalf("creating a template that exists ONLY in the artifact store returned %d (%v), "+
			"want 201 — a published template that is not also a compiled-in catalog id "+
			"cannot be booted anywhere in the fleet", code, out)
	}
	if !fetched {
		t.Fatalf("the create returned 201 without ever asking the store for %s", id)
	}
	if got, _ := out["template"].(string); got != id {
		t.Errorf("sandbox template = %q, want %q", got, id)
	}
}

// Unknown template is still 400; a compiled-in template the store lacks still boots.
func TestFetchClassificationLeavesTheOtherTwoOutcomesAlone(t *testing.T) {
	useStoreTier(t, func(_ context.Context, id string, _ string) error {
		return templates.ErrNotInStore
	})

	srv, key := newTestServer(t)
	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"golang-999"}`)
	if code != 400 {
		t.Errorf("an unknown template = %d (%v), want 400", code, out)
	}
	if code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base"}`); code != 201 {
		t.Errorf("a compiled-in template the store does not hold = %d (%v), want 201 — a node "+
			"that has published nothing has stopped working", code, out)
	}
}
