package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// The default test node runs the process runtime, which cannot capture, so a
// backup must refuse with 501 naming it — not 500, and not a half-done capture.
func TestBackupRoutesRefuseARuntimeThatCannotCapture(t *testing.T) {
	srv, key := newTestServer(t)
	id := createSandbox(t, srv.URL, key)

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/backups", key, "")
	if code != http.StatusNotImplemented {
		t.Fatalf("POST backups = %d, want 501 (%v)", code, out)
	}
	msg := fmt.Sprint(out["error"])
	if !strings.Contains(msg, "process") && !strings.Contains(msg, "object store") {
		t.Errorf("the refusal must name what is missing — the runtime or the store — got %q", msg)
	}
}

// A sandbox in another org does not exist, so every verb is 404, including the
// destructive one: it must not leak that the id is real.
func TestBackupRoutesAreOrgScoped(t *testing.T) {
	srv, st, key := newTestServerWithStore(t)
	id := createSandbox(t, srv.URL, key)

	_, otherKey, err := st.CreateKey(t.Context(), "other", "other-org")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/v1/sandboxes/" + id + "/backups", ""},
		{"GET", "/v1/sandboxes/" + id + "/backups", ""},
		{"DELETE", "/v1/sandboxes/" + id + "/backups/bkp_1", ""},
		{"POST", "/v1/sandboxes/" + id + "/backups/bkp_1/restore", `{"confirm":true}`},
	} {
		if code, out := do(t, c.method, srv.URL+c.path, otherKey, c.body); code != http.StatusNotFound {
			t.Errorf("%s %s as another org = %d, want 404 (%v)", c.method, c.path, code, out)
		}
	}
}

// Restore without confirm is a 400 with a code a client can branch on, and it
// is checked before the backup is looked up — otherwise a real id would sail
// through unconfirmed.
func TestRestoreWithoutConfirmIsRefusedFirst(t *testing.T) {
	srv, key := newTestServer(t)
	id := createSandbox(t, srv.URL, key)

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/backups/bkp_nope/restore", key, "{}")
	if code != http.StatusBadRequest {
		t.Fatalf("unconfirmed restore = %d, want 400 (%v)", code, out)
	}
	if out["code"] != "confirm_required" {
		t.Errorf("code = %v, want confirm_required so a client knows to prompt", out["code"])
	}
	if msg := strings.ToLower(fmt.Sprint(out["error"])); strings.Contains(msg, "not found") {
		t.Errorf("the refusal came from the unknown id, not the missing confirmation: %q", msg)
	}
}
