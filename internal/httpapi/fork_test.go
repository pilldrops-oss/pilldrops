package httpapi

import (
	"strings"
	"testing"
)

// The test server runs the process runtime, which cannot fork: the answer must
// be 501 naming the runtime, not a 503 that invites a retry.
func TestForkOnRuntimeWithoutForkIs501(t *testing.T) {
	srv, key := newTestServer(t)
	id := createSandbox(t, srv.URL, key)

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/fork", key, `{"name":"copy"}`)
	if code != 501 {
		t.Fatalf("fork = %d %v, want 501", code, out)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "cannot fork") {
		t.Fatalf("error = %q, want it to say the runtime cannot fork", msg)
	}
}

func TestForkUnknownSandboxIs404AndBadNameIs400(t *testing.T) {
	srv, key := newTestServer(t)
	if code, _ := do(t, "POST", srv.URL+"/v1/sandboxes/sb-nope/fork", key, ""); code != 404 {
		t.Fatalf("fork unknown = %d, want 404", code)
	}
	id := createSandbox(t, srv.URL, key)
	long := strings.Repeat("x", 65)
	if code, _ := do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/fork", key, `{"name":"`+long+`"}`); code != 400 {
		t.Fatalf("fork with a 65-rune name = %d, want 400", code)
	}
}
