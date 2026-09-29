package httpapi

import (
	"strings"
	"testing"
)

func TestCreateCarriesName(t *testing.T) {
	srv, key := newTestServer(t)
	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base","name":"my-worker"}`)
	if code != 201 {
		t.Fatalf("create = %d (%v)", code, out)
	}
	if out["name"] != "my-worker" {
		t.Fatalf("create response name = %v, want my-worker", out["name"])
	}
	id, _ := out["id"].(string)
	code, out = do(t, "GET", srv.URL+"/v1/sandboxes/"+id, key, "")
	if code != 200 || out["name"] != "my-worker" {
		t.Fatalf("get name = %d %v, want my-worker", code, out)
	}
}

func TestCreateWithoutNameDefaultsEmpty(t *testing.T) {
	srv, key := newTestServer(t)
	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base"}`)
	if code != 201 {
		t.Fatalf("create = %d (%v)", code, out)
	}
	if v, ok := out["name"]; ok && v != "" {
		t.Fatalf("name = %v, want empty", v)
	}
}

func TestRenamePersists(t *testing.T) {
	srv, key := newTestServer(t)
	_, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base"}`)
	id, _ := out["id"].(string)

	code, out := do(t, "PATCH", srv.URL+"/v1/sandboxes/"+id, key, `{"name":"renamed-box"}`)
	if code != 200 {
		t.Fatalf("rename = %d (%v)", code, out)
	}
	if out["name"] != "renamed-box" {
		t.Fatalf("rename response name = %v, want renamed-box", out["name"])
	}

	code, out = do(t, "GET", srv.URL+"/v1/sandboxes/"+id, key, "")
	if code != 200 || out["name"] != "renamed-box" {
		t.Fatalf("get after rename = %d %v, want renamed-box", code, out)
	}
}

func TestRenameTrimsWhitespace(t *testing.T) {
	srv, key := newTestServer(t)
	_, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base"}`)
	id, _ := out["id"].(string)

	code, out := do(t, "PATCH", srv.URL+"/v1/sandboxes/"+id, key, `{"name":"  padded  "}`)
	if code != 200 || out["name"] != "padded" {
		t.Fatalf("rename trim = %d %v, want 'padded'", code, out)
	}
}

func TestRenameEmptyClears(t *testing.T) {
	srv, key := newTestServer(t)
	_, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base","name":"has-a-name"}`)
	id, _ := out["id"].(string)

	code, out := do(t, "PATCH", srv.URL+"/v1/sandboxes/"+id, key, `{"name":""}`)
	if code != 200 || out["name"] != "" {
		t.Fatalf("rename clear = %d %v, want empty", code, out)
	}
	_, out = do(t, "GET", srv.URL+"/v1/sandboxes/"+id, key, "")
	if out["name"] != "" {
		t.Fatalf("get after clear = %v, want empty", out["name"])
	}
}

// Over-long names are rejected, not truncated.
func TestRenameRejectsLongName(t *testing.T) {
	srv, key := newTestServer(t)
	_, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base"}`)
	id, _ := out["id"].(string)

	long := strings.Repeat("a", 65)
	code, out := do(t, "PATCH", srv.URL+"/v1/sandboxes/"+id, key, `{"name":"`+long+`"}`)
	if code != 400 {
		t.Fatalf("rename over-long = %d (%v), want 400", code, out)
	}
}

func TestRenameAllowsExactly64(t *testing.T) {
	srv, key := newTestServer(t)
	_, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base"}`)
	id, _ := out["id"].(string)

	exact := strings.Repeat("b", 64)
	code, out := do(t, "PATCH", srv.URL+"/v1/sandboxes/"+id, key, `{"name":"`+exact+`"}`)
	if code != 200 || out["name"] != exact {
		t.Fatalf("rename exactly 64 = %d %v, want ok", code, out)
	}
}

func TestRenameStripsControlChars(t *testing.T) {
	srv, key := newTestServer(t)
	_, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base"}`)
	id, _ := out["id"].(string)

	code, out := do(t, "PATCH", srv.URL+"/v1/sandboxes/"+id, key, `{"name":"line1\nline2\ttab"}`)
	if code != 200 {
		t.Fatalf("rename control chars = %d (%v)", code, out)
	}
	if out["name"] != "line1line2tab" {
		t.Fatalf("rename control chars name = %q, want 'line1line2tab'", out["name"])
	}
}

func TestRenameUnknownSandbox404s(t *testing.T) {
	srv, key := newTestServer(t)
	code, _ := do(t, "PATCH", srv.URL+"/v1/sandboxes/nope", key, `{"name":"x"}`)
	if code != 404 {
		t.Fatalf("rename unknown = %d, want 404", code)
	}
}

// Cross-org is 404, not 403, so existence is not leaked.
func TestRenameCrossOrg404s(t *testing.T) {
	srv, st, key := newTestServerWithStore(t)
	_, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base"}`)
	id, _ := out["id"].(string)

	_, otherKey, err := st.CreateKey(t.Context(), "other", "other-org")
	if err != nil {
		t.Fatalf("other key: %v", err)
	}
	code, _ := do(t, "PATCH", srv.URL+"/v1/sandboxes/"+id, otherKey, `{"name":"steal"}`)
	if code != 404 {
		t.Fatalf("cross-org rename = %d, want 404", code)
	}
}
