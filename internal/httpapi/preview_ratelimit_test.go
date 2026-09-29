package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/auth/previewtoken"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
)

func TestPreviewIsRateLimitedPerIP(t *testing.T) {
	t.Setenv("PREVIEW_RATE_RPS", "1")
	t.Setenv("PREVIEW_RATE_BURST", "1")
	srv, _ := newTestServer(t)

	got429 := false
	for i := 0; i < 5; i++ {
		resp, err := http.Get(srv.URL + "/preview/nope/8000/")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			got429 = true
		}
	}
	if !got429 {
		t.Fatal("no 429 after 5 rapid /preview requests with burst=1 — preview is not rate limited")
	}
}

// Behind the fleet's front door every visitor arrives from the same address, so
// on a clustered node a request with a valid hop token skips the per-IP bucket
// (the front door limits per visitor). Anything else, and every request on a
// standalone node, is still limited.
func TestClusteredNodeDoesNotThrottleTheFrontDoorAsOneVisitor(t *testing.T) {
	t.Setenv("PREVIEW_RATE_RPS", "1")
	t.Setenv("PREVIEW_RATE_BURST", "1")
	d, st := newDaemonForPreviewTest(t)
	s := New(d, st)
	s.SetPreviewSecret([]byte(testPreviewSecret))
	sb, err := d.Create(context.Background(), daemon.CreateSpec{Template: "base", OrgID: "u:cust1"})
	if err != nil {
		t.Fatal(err)
	}
	tok := previewtoken.Token([]byte(testPreviewSecret), sb.ID, 8000, "u:cust1")
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	count429 := func(h http.Handler, query string) int {
		n := 0
		for i := 0; i < 10; i++ {
			rw := httptest.NewRecorder()
			h.ServeHTTP(rw, httptest.NewRequest("GET", "/preview/"+sb.ID+"/8000/"+query, nil))
			if rw.Code == http.StatusTooManyRequests {
				n++
			}
		}
		return n
	}
	if count429(s.throttlePreview(ok), "?ptok="+tok) == 0 {
		t.Fatal("a standalone node stopped rate limiting its visitors")
	}
	s.SetCluster("http://orchestrator.invalid", "orch-key")
	if n := count429(s.throttlePreview(ok), "?ptok="+tok); n != 0 {
		t.Fatalf("clustered node throttled front-door traffic carrying a valid hop token (%d/10 refused)", n)
	}
	if count429(s.throttlePreview(ok), "?ptok=forged") == 0 {
		t.Fatal("a forged token skipped the throttle")
	}
}
