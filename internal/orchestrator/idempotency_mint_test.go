package orchestrator

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// A create the caller sent no Idempotency-Key on must reach the node without
// one. Against the old code this fails: the tier minted a key for every create,
// which bought no retry anybody could make and put one more row — and one more
// way to answer 503 — on the path of every single create.
func TestACreateWithNoCallerKeyCarriesNone(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/sandboxes", strings.NewReader(`{"template":"base"}`))
	if got := strings.TrimSpace(r.Header.Get("Idempotency-Key")); got != "" {
		t.Fatalf("the fixture already carries a key %q", got)
	}
	if got := idempotencyKey(keyedCreateContext(r)); got != "" {
		t.Fatalf("a create with no caller key would be forwarded with Idempotency-Key %q", got)
	}
}

// The caller's own key still travels: theirs is a guarantee they asked for.
func TestACallerKeyStillTravelsToTheNode(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/sandboxes", strings.NewReader(`{"template":"base"}`))
	r.Header.Set("Idempotency-Key", "  customer-chosen-1  ")
	if got := idempotencyKey(keyedCreateContext(r)); got != "customer-chosen-1" {
		t.Fatalf("forwarded key = %q, want the caller's, trimmed", got)
	}
}
