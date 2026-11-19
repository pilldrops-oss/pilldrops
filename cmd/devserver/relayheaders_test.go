package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A 429 or 503 from the control plane is only useful with its Retry-After; the
// buffered proxy used to return the body and Content-Type and drop the rest.
func TestABufferedReplyKeepsItsRetryAfter(t *testing.T) {
	orch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "12")
		w.Header().Set("X-Internal", "node-9")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"test sends are limited"}`))
	}))
	defer orch.Close()

	in := httptest.NewRequest("POST", "/v1/webhooks/whk_1/test", nil)
	body, status, header := proxyToOrchestrator(in, orch.URL, "orch-key", "org_1")
	rec := httptest.NewRecorder()
	relayResponseHeaders(rec, header)
	rec.WriteHeader(status)
	_, _ = rec.Write(body)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "12" {
		t.Errorf("Retry-After %q, want 12", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type %q, want application/json", got)
	}
	if got := rec.Header().Get("X-Internal"); got != "" {
		t.Errorf("an internal header was relayed: X-Internal=%q", got)
	}
}
