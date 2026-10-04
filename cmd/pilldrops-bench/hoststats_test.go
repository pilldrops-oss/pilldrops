package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// GET /v1/host is operator-only; a refusal must stop the run, not read as empty stats.
func TestHostStatsNeedsTheOperatorKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer op" {
			http.Error(w, `{"error":"operator role required"}`, http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`{"cpu_cores":8}`))
	}))
	defer srv.Close()
	cl := client{c: srv.Client()}
	*host, *key = srv.URL, "tenant"

	*operatorKey = ""
	if _, err := cl.hostStats(); err == nil || !strings.Contains(err.Error(), "operator key") {
		t.Errorf("tenant key: err = %v, want one naming the operator key", err)
	}
	*operatorKey = "op"
	hs, err := cl.hostStats()
	if err != nil || hs["cpu_cores"] != float64(8) {
		t.Errorf("operator key: %v, %v", hs, err)
	}
}
