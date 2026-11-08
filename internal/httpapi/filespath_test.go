package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// A path the sandbox will refuse every time is the caller's, not the node's.
// `cp f.txt sb:/tmp/` answered
//
//	invalid path (HTTP 503)
//	hint: server-side error — retry
//
// so the CLI told people to retry something that can only fail. The runtimes'
// own refusal now carries rt.ErrPathRefused and the write handler answers 400
// naming the rule; this pins the status the handler has always owned.
func TestAPathOutsideTheWorkspaceIsAClientError(t *testing.T) {
	srv, key := newTestServer(t)
	id := createSandbox(t, srv.URL, key)

	if code, out := do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/files?path=../../etc/passwd", key, "x"); code != http.StatusBadRequest {
		t.Fatalf("write outside the workspace = %d, want 400 (%v)", code, out)
	}
}

// An absolute path is taken as given, never re-rooted under the home: /tmp/x.js
// used to answer 200 and land at ~/tmp/x.js, where `ls /tmp/x.js` finds nothing.
func TestAnAbsolutePathOutsideTheWorkspaceIsRefusedNotRelocated(t *testing.T) {
	srv, key := newTestServer(t)
	id := createSandbox(t, srv.URL, key)
	files := srv.URL + "/v1/sandboxes/" + id + "/files?path="

	code, out := do(t, "POST", files+"/tmp/x.js", key, "x")
	if code != http.StatusBadRequest {
		t.Fatalf("write /tmp/x.js = %d %v, want 400", code, out)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "relative to the sandbox home") {
		t.Errorf("refusal %q does not name the rule", msg)
	}
	if code, _ := getText(t, files+"tmp/x.js", key); code != http.StatusNotFound {
		t.Errorf("read of the relocated path = %d, want 404 (nothing written)", code)
	}
}
