package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
