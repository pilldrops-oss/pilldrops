package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func noProbe(t *testing.T) func() (int, error) {
	t.Helper()
	return func() (int, error) {
		t.Fatal("probe must not run for this state")
		return 0, nil
	}
}

func TestLocalPreviewStateProcessFacts(t *testing.T) {
	// States the reaper decides; a probe could only muddy a known answer.
	if st, _, _ := localPreviewState("paused", true, noProbe(t)); st != "paused" {
		t.Fatalf("paused → %q", st)
	}
	if st, _, _ := localPreviewState("crashed", true, noProbe(t)); st != "app_error" {
		t.Fatalf("crashed → %q, want app_error", st)
	}
	if st, _, _ := localPreviewState("exited", true, noProbe(t)); st != "terminal" {
		t.Fatalf("exited → %q, want terminal", st)
	}
	// No app ever started: probing the built-in surface would be a false green.
	if st, _, _ := localPreviewState("running", false, noProbe(t)); st != "terminal" {
		t.Fatalf("bare running → %q, want terminal", st)
	}
}

func TestLocalPreviewStateProbeClassification(t *testing.T) {
	cases := []struct {
		code int
		err  error
		want string
	}{
		{200, nil, "ready"},
		{302, nil, "ready"},
		{401, nil, "ready"}, // an app answering 401 is answering
		{404, nil, "no_page"},
		{502, nil, "starting"},
		{503, nil, "starting"},
		{500, nil, "app_error"},
		{0, errors.New("connection refused"), "starting"},
	}
	for _, c := range cases {
		st, up, has := localPreviewState("running", true, func() (int, error) { return c.code, c.err })
		if st != c.want {
			t.Errorf("code=%d err=%v → %q, want %q", c.code, c.err, st, c.want)
		}
		if c.err == nil && (!has || up != c.code) {
			t.Errorf("code=%d: upstream not reported", c.code)
		}
		if c.err != nil && has {
			t.Errorf("failed probe must not invent an upstream status")
		}
	}
}

func TestLocalPreviewProbeAgainstRealServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer srv.Close()
	var port int
	if _, err := fmtSscanf(srv.URL, &port); err != nil {
		t.Skipf("could not parse port from %s", srv.URL)
	}
	code, err := localPreviewProbe(t.Context(), port)
	if err != nil || code != http.StatusTeapot {
		t.Fatalf("probe = %d, %v", code, err)
	}
}

// fmtSscanf pulls the port out of an httptest URL (http://127.0.0.1:PORT).
func fmtSscanf(url string, port *int) (int, error) {
	i := strings.LastIndex(url, ":")
	if i < 0 {
		return 0, errors.New("no port")
	}
	n := 0
	for _, ch := range url[i+1:] {
		if ch < '0' || ch > '9' {
			return 0, errors.New("bad port")
		}
		n = n*10 + int(ch-'0')
	}
	*port = n
	return 1, nil
}

// The local preview reaches only a live sandbox's own app port, with the
// request intact; the terminal port is its owner's alone.
func TestLocalPreviewOnlyReachesTheSandboxApp(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, _ = fmt.Fprintf(w, "%s %s?%s %s", r.Method, r.URL.Path, r.URL.RawQuery, body)
	}))
	defer app.Close()
	var port int
	if _, err := fmtSscanf(app.URL, &port); err != nil {
		t.Fatal(err)
	}
	mgr := &DevManager{sandboxes: map[string]*DevSandbox{"sb-1": {ID: "sb-1", UserID: "u:a", Port: port}}}
	serve := func(owner, method, path, body string) *httptest.ResponseRecorder {
		r := chi.NewRouter()
		h := localPreview(mgr, func(*http.Request) string { return owner })
		r.Handle("/preview/{id}/{port}/*", h)
		r.Handle("/preview/{id}/{port}", h)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		return w
	}

	if w := serve("u:a", "POST", "/preview/sb-1/8000/api/x?q=1", "payload"); w.Body.String() != "POST /api/x?q=1 payload" {
		t.Errorf("app saw %q, want method, path, query and body intact", w.Body.String())
	}
	if w := serve("u:a", "GET", "/preview/nope/8080/health", ""); w.Code != http.StatusNotFound {
		t.Errorf("unknown sandbox = %d, want 404 rather than a proxy to localhost:8080", w.Code)
	}
	if w := serve("u:b", "GET", "/preview/sb-1/7681/ws", ""); w.Code != http.StatusNotFound {
		t.Errorf("another tenant's terminal = %d, want 404", w.Code)
	}
}
