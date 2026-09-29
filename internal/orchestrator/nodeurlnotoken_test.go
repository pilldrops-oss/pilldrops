package orchestrator

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// noTokenNodeFixture is a node whose preview links carry no ?ptok= — what a
// node mints whenever it has no preview secret to sign with. The links are
// otherwise identical, and they still name the node's own host.
//
// It answers PATCH as well: set-port's whole answer is the new preview link.
func noTokenNodeFixture(t *testing.T) *Server {
	t.Helper()
	const body = `{"id":"sb-1","template":"base","state":"running","has_desktop":true,` +
		`"url":"http://node.invalid/preview/sb-1/8000/",` +
		`"terminal_url":"http://node.invalid/preview/sb-1/7681/",` +
		`"desktop_url":"http://node.invalid/preview/sb-1/6080/"}`
	s, _ := createFailureFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes",
			r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sb-1",
			r.Method == http.MethodPatch && r.URL.Path == "/v1/sandboxes/sb-1":
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodPost {
				w.WriteHeader(http.StatusCreated)
			}
			_, _ = io.WriteString(w, body)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	return s
}

// The rewrite used to fire only on links carrying the node's hop credential, on
// the reasoning that the credential is what marks a link as the node's. It is
// not: a node that mints no token stamps its own host on desktop_url and
// terminal_url all the same, and those were relayed untouched. Measured on a
// live firecracker node — `sandbox get` answered with url on the control plane
// and desktop_url and terminal_url on 127.0.0.1:18901, which on a fleet is an
// address the caller has no route to.
func TestNodeLinksWithNoCredentialAreStillRewritten(t *testing.T) {
	s := noTokenNodeFixture(t)

	w := doCreate(t, s, `{"template":"base","vcpu":1,"mem_mb":512}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body %s", w.Code, w.Body.String())
	}
	assertNoNodeCredential(t, "create", w.Body.Bytes())

	for _, tc := range []struct {
		what, method, body string
	}{
		{"get", http.MethodGet, ""},
		// set-port and rename both come through here, and the answer used to be
		// relayed byte for byte.
		{"patch", http.MethodPatch, `{"port":3000}`},
	} {
		var rdr io.Reader
		if tc.body != "" {
			rdr = strings.NewReader(tc.body)
		}
		req := httptest.NewRequest(tc.method, "/v1/sandboxes/sb-1", rdr)
		req.Header.Set("Authorization", "Bearer api-key")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, body %s", tc.what, rec.Code, rec.Body.String())
		}
		assertNoNodeCredential(t, tc.what, rec.Body.Bytes())

		var got map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode %s response: %v", tc.what, err)
		}
		for _, k := range []string{"url", "desktop_url", "terminal_url"} {
			v, _ := got[k].(string)
			if v == "" {
				t.Errorf("%s: %s is missing from the answer", tc.what, k)
			}
		}
	}
}
