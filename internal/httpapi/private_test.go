package httpapi

import (
	"net/http"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/runtime/runtimetest"
)

func TestPatchPrivate(t *testing.T) {
	for _, supported := range []bool{false, true} {
		fake := runtimetest.NewFake()
		fake.PrivateAccess = supported
		srv, key := newFakeRuntimeServer(t, fake)
		code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base"}`)
		if code != 201 {
			t.Fatalf("create: %d %v", code, out)
		}
		id, _ := out["id"].(string)
		code, out = do(t, "PATCH", srv.URL+"/v1/sandboxes/"+id, key, `{"private":[{"name":"db","port":5432}]}`)
		if !supported {
			if code != http.StatusConflict || out["code"] != codePrivateAccessUnsupported {
				t.Fatalf("unsupported node: %d %v, want 409 %s", code, out, codePrivateAccessUnsupported)
			}
			if msg, _ := out["error"].(string); contains(msg, "fake") {
				t.Errorf("error %q names the runtime", msg)
			}
			continue
		}
		if code != 200 || len(fake.Private[id]) != 1 {
			t.Fatalf("supported node: %d %v, runtime got %+v", code, out, fake.Private[id])
		}
		if code, out := do(t, "PATCH", srv.URL+"/v1/sandboxes/"+id, key, `{"private":[{"name":"Bad Name","port":1}]}`); code != 400 {
			t.Fatalf("invalid target: %d %v, want 400", code, out)
		}
	}
}
