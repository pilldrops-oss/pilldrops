package httpapi

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func createBox(t *testing.T, url, key, body string) string {
	t.Helper()
	code, out := do(t, "POST", url+"/v1/sandboxes", key, body)
	if code != http.StatusCreated {
		t.Fatalf("create = %d (%v)", code, out)
	}
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("create returned no id: %v", out)
	}
	return id
}

// expiresAt reads a sandbox's expiry through the API; nil means no expiry.
func expiresAt(t *testing.T, url, key, id string) any {
	t.Helper()
	code, out := do(t, "GET", url+"/v1/sandboxes/"+id, key, "")
	if code != http.StatusOK {
		t.Fatalf("get %s = %d (%v)", id, code, out)
	}
	return out["expires_at"]
}

// A free plan must not buy a never-expiring sandbox through /timeout.
func TestTimeoutCannotBuyPersistenceOnFreePlan(t *testing.T) {
	srv, _, key := newBilledTestServer(t)
	id := createBox(t, srv.URL, key, `{"template":"base"}`)

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/timeout", key, `{"seconds":-1}`)
	if code != http.StatusForbidden {
		t.Fatalf("free plan bought persistence via /timeout: %d (%v), want 403", code, out)
	}
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "free") || !strings.Contains(msg, "upgrade") {
		t.Errorf("refusal must name the plan and the way out, got %q", msg)
	}
	if exp := expiresAt(t, srv.URL, key, id); exp == nil {
		t.Error("sandbox has no expiry after a refused request — the refusal did not hold")
	}
}

func TestTimeoutClampsToPlanSessionTTL(t *testing.T) {
	srv, _, key := newBilledTestServer(t)
	id := createBox(t, srv.URL, key, `{"template":"base"}`)

	before := time.Now()
	code, out := do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/timeout", key, `{"seconds":999999999}`)
	if code != http.StatusOK {
		t.Fatalf("timeout = %d (%v)", code, out)
	}
	raw, _ := expiresAt(t, srv.URL, key, id).(string)
	if raw == "" {
		t.Fatal("sandbox came back with no expiry — a positive timeout must still expire")
	}
	exp, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("expires_at %q: %v", raw, err)
	}
	// Free is SessionTTLHrs=4.
	if d := exp.Sub(before); d > 4*time.Hour+time.Minute || d < 4*time.Hour-time.Minute {
		t.Errorf("expiry is %v out, want ~4h (the free plan's session TTL)", d)
	}
}

func TestTimeoutGrantsPersistenceOnAPersistentPlan(t *testing.T) {
	srv, st, key := newBilledTestServer(t)
	usePlan(t, st, "default", "pro") // 1 workstation
	id := createBox(t, srv.URL, key, `{"template":"base"}`)

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/timeout", key, `{"seconds":-1}`)
	if code != http.StatusOK {
		t.Fatalf("promotion on pro = %d (%v), want 200", code, out)
	}
	if exp := expiresAt(t, srv.URL, key, id); exp != nil {
		t.Errorf("expires_at = %v, want null — the promotion did not take", exp)
	}

	// Re-asserting persistence must not count the sandbox against its own cap.
	if code, out := do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/timeout", key, `{"seconds":-1}`); code != http.StatusOK {
		t.Errorf("re-asserting persistence = %d (%v), want 200", code, out)
	}
}

func TestTimeoutRespectsWorkstationCap(t *testing.T) {
	srv, st, key := newBilledTestServer(t)
	usePlan(t, st, "default", "pro") // 1 workstation included

	first := createBox(t, srv.URL, key, `{"template":"base","timeout":-1}`)
	if exp := expiresAt(t, srv.URL, key, first); exp != nil {
		t.Fatal("first workstation is not persistent — the cap test proves nothing")
	}

	second := createBox(t, srv.URL, key, `{"template":"base"}`)
	code, out := do(t, "POST", srv.URL+"/v1/sandboxes/"+second+"/timeout", key, `{"seconds":-1}`)
	if code != http.StatusTooManyRequests {
		t.Fatalf("second workstation via /timeout = %d (%v), want 429", code, out)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "upgrade") {
		t.Errorf("refusal does not say how to fix it: %q", msg)
	}
	if exp := expiresAt(t, srv.URL, key, second); exp == nil {
		t.Error("the refused sandbox lost its expiry anyway")
	}
}

func TestTimeoutWithinPlanIsHonoured(t *testing.T) {
	srv, _, key := newBilledTestServer(t)
	id := createBox(t, srv.URL, key, `{"template":"base"}`)

	before := time.Now()
	if code, out := do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/timeout", key, `{"seconds":600}`); code != http.StatusOK {
		t.Fatalf("timeout = %d (%v)", code, out)
	}
	raw, _ := expiresAt(t, srv.URL, key, id).(string)
	exp, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("expires_at %q: %v", raw, err)
	}
	if d := exp.Sub(before); d > 11*time.Minute || d < 9*time.Minute {
		t.Errorf("expiry is %v out, want ~10m — an in-plan request must be honoured as asked", d)
	}
}
