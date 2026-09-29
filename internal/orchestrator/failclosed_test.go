package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

// s.auth, the heartbeat guard and authorizePreview must refuse, not serve, when
// their key or token is unset, except under the DEV_INSECURE escape hatch.

func TestOrchestratorAuthFailsClosedWhenAPIKeyUnset(t *testing.T) {
	s := NewServer("", "t") // apiKey unset
	req := httptest.NewRequest("GET", "/v1/nodes", nil)
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Fatalf("unauthenticated request with no API key configured got 200 (fail-open); want a refusal")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("apiKey unset, no DEV_INSECURE: got %d, want 503 (not configured)", w.Code)
	}
}

func TestOrchestratorAuthDevInsecureEscapeHatch(t *testing.T) {
	t.Setenv("DEV_INSECURE", "1")
	s := NewServer("", "t")
	req := httptest.NewRequest("GET", "/v1/nodes", nil)
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("DEV_INSECURE=1 with apiKey unset: got %d, want 200 (dev escape hatch honored)", w.Code)
	}
}

func TestOrchestratorAuthDevInsecureDoesNotWeakenConfiguredKey(t *testing.T) {
	t.Setenv("DEV_INSECURE", "1")
	s := NewServer("realkey", "t")
	req := httptest.NewRequest("GET", "/v1/nodes", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong bearer with a configured key (even under DEV_INSECURE) = %d, want 401", w.Code)
	}
}

// A refused heartbeat must also leave the registry untouched.
func TestHeartbeatRefusedWhenNodeTokenUnset(t *testing.T) {
	s := NewServer("k", "") // nodeToken unset
	req := httptest.NewRequest("POST", "/internal/heartbeat",
		strings.NewReader(`{"id":"rogue","url":"http://evil"}`))
	w := httptest.NewRecorder()
	s.handleHeartbeat(w, req)
	if w.Code == http.StatusOK {
		t.Fatalf("heartbeat with no node token configured got 200 (fail-open); want a refusal")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("nodeToken unset, no DEV_INSECURE: got %d, want 503 (not configured)", w.Code)
	}
	if _, ok := s.reg.Node("rogue"); ok {
		t.Fatal("a refused heartbeat still registered the node")
	}
}

func TestHeartbeatDevInsecureEscapeHatch(t *testing.T) {
	t.Setenv("DEV_INSECURE", "1")
	s := NewServer("k", "")
	req := httptest.NewRequest("POST", "/internal/heartbeat",
		strings.NewReader(`{"id":"dev-node","url":"http://x"}`))
	w := httptest.NewRecorder()
	s.handleHeartbeat(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("DEV_INSECURE=1 with nodeToken unset: got %d, want 200", w.Code)
	}
	if _, ok := s.reg.Node("dev-node"); !ok {
		t.Fatal("heartbeat accepted under DEV_INSECURE but node was not registered")
	}
}

func TestPreviewFailsClosedWhenAPIKeyUnset(t *testing.T) {
	s := NewServer("", "t") // apiKey unset, so previewToken("sb1") == ""
	req := httptest.NewRequest("GET", "/preview/sb1/8000/", nil)
	w := httptest.NewRecorder()
	if ok := s.authorizePreview(w, req, "sb1", 8000); ok {
		t.Fatal("authorizePreview allowed a request with no API key configured (fail-open); want refused")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("apiKey unset, no DEV_INSECURE: got %d, want 503 (not configured)", w.Code)
	}
}

func TestPreviewDevInsecureEscapeHatch(t *testing.T) {
	t.Setenv("DEV_INSECURE", "1")
	s := NewServer("", "t")
	req := httptest.NewRequest("GET", "/preview/sb1/8000/", nil)
	w := httptest.NewRecorder()
	if ok := s.authorizePreview(w, req, "sb1", 8000); !ok {
		t.Fatalf("DEV_INSECURE=1 with apiKey unset: authorizePreview refused, want allowed")
	}
}

func TestPreviewStillGatesWhenAPIKeyConfigured(t *testing.T) {
	s := NewServer("k", "t")
	req := httptest.NewRequest("GET", "/preview/sb1/8000/", nil) // no ?token=
	w := httptest.NewRecorder()
	if ok := s.authorizePreview(w, req, "sb1", 8000); ok {
		t.Fatal("authorizePreview allowed a request with no token even though an API key IS configured")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("missing token with a configured key: got %d, want 403 (the normal private-preview refusal)", w.Code)
	}
}

func TestSignedOrgOverrideRefusedWhenNodeTokenUnset(t *testing.T) {
	s := NewServer("k", "") // nodeToken unset, nothing to sign with
	if hdr, ok := s.signedOrgOverride("u:cust9", "node-a", "GET", "/v1/sandboxes"); ok {
		t.Fatalf("signedOrgOverride succeeded with no secret: hdr=%q, want refused", hdr)
	}
}

func TestSignedOrgOverrideDevInsecureFallsBackToBare(t *testing.T) {
	t.Setenv("DEV_INSECURE", "1")
	s := NewServer("k", "")
	hdr, ok := s.signedOrgOverride("u:cust9", "node-a", "GET", "/v1/sandboxes")
	if !ok || hdr != "u:cust9" {
		t.Fatalf("signedOrgOverride under DEV_INSECURE = (%q, %v), want (u:cust9, true)", hdr, ok)
	}
}

// Dropping the override would run the request against the wrong org, so the
// call must fail outright.
func TestForwardRefusesToRelayUnsignedOrgOverride(t *testing.T) {
	sawHeader := ""
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawHeader = r.Header.Get("X-Org-Override")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"sb-1"}`))
	}))
	defer node.Close()

	s := NewServer("k", "") // nodeToken unset, DEV_INSECURE unset
	upsert(s.reg, "node-a", node.URL, "kk", "", nil, "", cluster.Capacity{Cores: 8, MemTotalMB: 64000, MemAvailMB: 64000}, time.Now())

	req := httptest.NewRequest("POST", "/v1/sandboxes", strings.NewReader(`{"template":"base"}`))
	req.Header.Set("X-Org-Override", "u:cust9")
	w := httptest.NewRecorder()
	s.handleCreate(w, req)

	if w.Code == http.StatusCreated {
		t.Fatalf("create succeeded (%d) despite no signing secret — the org override must have been relayed unsigned", w.Code)
	}
	if sawHeader != "" {
		t.Fatalf("node received X-Org-Override=%q despite the orchestrator having no signing secret (bare relay leaked through)", sawHeader)
	}
}
