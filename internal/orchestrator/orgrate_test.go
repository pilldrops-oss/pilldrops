package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

func TestOrgRateLimitsTheCustomerNotTheOperator(t *testing.T) {
	s := NewServer("op-key", "node-token")
	s.orgRate, _ = cluster.NewOrgRate(2, "")
	h := s.auth(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	call := func(org string) int {
		r := httptest.NewRequest("GET", "/v1/sandboxes", nil)
		r.Header.Set("Authorization", "Bearer op-key")
		if org != "" {
			r.Header.Set(orgOverride, org)
		}
		w := httptest.NewRecorder()
		h(w, r)
		return w.Code
	}
	first, second := call("u:a"), call("u:a")
	if first != 204 || second != 204 {
		t.Fatalf("refused under the limit: %d, %d", first, second)
	}
	if got := call("u:a"); got != http.StatusTooManyRequests {
		t.Fatalf("3rd request for one org in a second = %d, want 429", got)
	}
	if call("u:b") != 204 {
		t.Fatal("one org's burst limited another")
	}
	for i := 0; i < 5; i++ {
		if call("") != 204 {
			t.Fatal("the operator's own traffic, which names no org, was limited")
		}
	}
}
