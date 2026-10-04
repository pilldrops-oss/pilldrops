package httpapi

import (
	"net/http"
	"testing"
)

// kill(2) reads 0 as the caller's process group and -1 as every process; 0 is
// also the pid a runtime lists for a process it cannot address.
func TestSignalRefusesAPidThatIsNotAProcess(t *testing.T) {
	srv, key := newTestServer(t)
	id := createBox(t, srv.URL, key, `{"template":"base"}`)
	for _, body := range []string{`{"pid":0,"signal":"KILL"}`, `{"pid":-1,"signal":"KILL"}`, `{"signal":"KILL"}`} {
		if code, out := do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/process/signal", key, body); code != http.StatusBadRequest {
			t.Errorf("%s = %d (%v), want 400", body, code, out)
		}
	}
}
