package httpapi

import (
	"strings"
	"testing"
)

func TestMetricsLatencyHistogram(t *testing.T) {
	srv, key := newOperatorTestServer(t)
	if code, _ := do(t, "GET", srv.URL+"/v1/templates", key, ""); code != 200 {
		t.Fatalf("seed request = %d, want 200", code)
	}

	code, body := getText(t, srv.URL+"/metrics", key)
	if code != 200 {
		t.Fatalf("metrics = %d, want 200", code)
	}

	for _, want := range []string{
		"# TYPE sandboxd_http_request_duration_seconds histogram",
		"sandboxd_http_request_duration_seconds_bucket{le=",
		`sandboxd_http_request_duration_seconds_bucket{le="+Inf"}`,
		"sandboxd_http_request_duration_seconds_sum",
		"sandboxd_http_request_duration_seconds_count",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics missing %q", want)
		}
	}
}
