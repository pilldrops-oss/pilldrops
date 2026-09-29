package httpapi

import (
	"net/http"
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
