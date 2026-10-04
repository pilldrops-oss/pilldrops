package httpapi

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestMetricsExposeOperationalSeries(t *testing.T) {
	srv, key := newOperatorTestServer(t)
	defer srv.Close()

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	out := string(body)

	for _, want := range []string{
		"sandboxd_reconcile_duration_seconds_bucket",
		"sandboxd_reconcile_overruns_total",
		"sandboxd_sandbox_pause_duration_seconds_bucket",
		"sandboxd_sandbox_resume_duration_seconds_bucket",
		"sandboxd_scanner_duration_seconds_bucket",
		"sandboxd_store_conns_in_use",
		"sandboxd_store_conns_wait_total",
		"go_goroutines",
		"go_memstats_alloc_bytes",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("/metrics is missing %q", want)
		}
	}

	// Every series must declare a TYPE.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "# HELP ") {
			name := strings.Fields(line)[2]
			if !strings.Contains(out, "# TYPE "+name+" ") {
				t.Errorf("series %q has HELP but no TYPE", name)
			}
		}
	}
}
