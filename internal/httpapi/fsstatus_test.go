package httpapi

import (
	"net/http"
	"testing"
)

// API B15: read, stat and list answered 404 for every failure and the
// mutations 503, so a mkdir over an existing file told the caller to retry.
func TestFilesystemErrorsNameWhatIsWrongWithThePath(t *testing.T) {
	srv, key := newTestServer(t)
	code, created := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base"}`)
	if code != 201 {
		t.Fatalf("create = %d %v", code, created)
	}
	fsURL := srv.URL + "/v1/sandboxes/" + created["id"].(string)
	for _, step := range []string{`{"path":"dir/sub"}`, `{"path":"other/sub"}`} {
		if code, out := do(t, "POST", fsURL+"/filesystem/mkdir", key, step); code != 200 {
			t.Fatalf("setup mkdir %s = %d %v", step, code, out)
		}
	}
	if code, out := do(t, "POST", fsURL+"/files?path=afile", key, "x"); code != 200 {
		t.Fatalf("setup write = %d %v", code, out)
	}

	for _, tc := range []struct {
		name, method, path, body string
		want                     int
	}{
		{"stat of a missing path", "GET", "/filesystem/stat?path=nope", "", http.StatusNotFound},
		{"list of a missing dir", "GET", "/filesystem/list?path=nope", "", http.StatusNotFound},
		{"read of a missing file", "GET", "/files?path=nope", "", http.StatusNotFound},
		{"move onto a non-empty dir", "POST", "/filesystem/move", `{"src":"other","dst":"dir"}`, http.StatusConflict},
		{"list of a file", "GET", "/filesystem/list?path=afile", "", http.StatusBadRequest},
		{"mkdir through a file", "POST", "/filesystem/mkdir", `{"path":"afile/sub"}`, http.StatusBadRequest},
		{"remove of the home itself", "POST", "/filesystem/remove", `{"path":"."}`, http.StatusBadRequest},
	} {
		if code, out := do(t, tc.method, fsURL+tc.path, key, tc.body); code != tc.want {
			t.Errorf("%s = %d %v, want %d", tc.name, code, out, tc.want)
		}
	}
}
