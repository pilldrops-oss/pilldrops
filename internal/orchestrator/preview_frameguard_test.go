package orchestrator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

// frameGuardFor proxies a preview request to a fake node returning status and
// returns the headers the browser would see.
func frameGuardFor(t *testing.T, sandboxDomain string, status int) http.Header {
	t.Helper()
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// The upstream app's own permissive policy must not win.
		w.Header().Set("Content-Security-Policy", "frame-ancestors *")
		w.Header().Set("Referrer-Policy", "unsafe-url")
		w.WriteHeader(status)
		_, _ = w.Write([]byte("app"))
	}))
	t.Cleanup(node.Close)

	s := NewServer("k", "t")
	s.sandboxDomain = sandboxDomain
	upsert(s.reg, "node-a", node.URL, "kk", "", nil, "", cluster.Capacity{}, time.Now())
	s.reg.Place("sb1", "node-a")

	tok := s.previewToken(context.Background(), "sb1", 8000)
	req := httptest.NewRequest("GET", "/preview/sb1/8000/?token="+tok, nil)
	req.SetPathValue("id", "sb1")
	req.SetPathValue("rest", "8000/")
	w := httptest.NewRecorder()
	s.handlePreview(w, req)
	return w.Result().Header
}

func TestPreviewFrameGuardAllowsTheDashboard(t *testing.T) {
	h := frameGuardFor(t, "pilldrops.com", 200)
	csp := h.Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors") {
		t.Fatalf("no frame-ancestors directive: %q", csp)
	}
	if !strings.Contains(csp, "pilldrops.com") {
		t.Errorf("policy does not admit the dashboard origin: %q", csp)
	}
	if strings.Contains(csp, "frame-ancestors *") {
		t.Errorf("upstream's permissive policy leaked through: %q", csp)
	}
	if xfo := h.Get("X-Frame-Options"); xfo == "DENY" {
		t.Errorf("X-Frame-Options=DENY contradicts the allow-list policy")
	}
}

func TestPreviewFrameGuardDeniesWhenSameOrigin(t *testing.T) {
	h := frameGuardFor(t, "", 200)
	csp := h.Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("want frame-ancestors 'none', got %q", csp)
	}
	if h.Get("X-Frame-Options") != "DENY" {
		t.Errorf("want X-Frame-Options DENY for older browsers, got %q", h.Get("X-Frame-Options"))
	}
}

// The boot-window 503 replaces the header map wholesale.
func TestPreviewFrameGuardAppliesToTheBootingPage(t *testing.T) {
	h := frameGuardFor(t, "", 502)
	if !strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatalf("booting page missing the frame guard: %q", h.Get("Content-Security-Policy"))
	}
}

func TestPreviewFrameGuardAppliesToTheRefusalPage(t *testing.T) {
	s := NewServer("k", "t")
	s.sandboxDomain = ""
	req := httptest.NewRequest("GET", "/preview/sb1/8000/", nil) // no token
	req.SetPathValue("id", "sb1")
	req.SetPathValue("rest", "8000/")
	w := httptest.NewRecorder()
	if s.authorizePreview(w, req, "sb1", 8000) {
		t.Fatal("expected the request to be refused")
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Errorf("refusal page missing the frame guard: %q", w.Header().Get("Content-Security-Policy"))
	}
}

func TestPreviewSendsNoReferrer(t *testing.T) {
	h := frameGuardFor(t, "pilldrops.com", 200)
	if got := h.Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("Referrer-Policy = %q, want no-referrer — the ?token= in this URL "+
			"would reach any third party the sandbox app links to or loads from", got)
	}
}

func TestPreviewAppCannotOptOutOfNoReferrer(t *testing.T) {
	h := frameGuardFor(t, "pilldrops.com", 200)
	if got := h.Values("Referrer-Policy"); len(got) != 1 || got[0] != "no-referrer" {
		t.Fatalf("Referrer-Policy = %v, want exactly [no-referrer] — the upstream set "+
			"unsafe-url and a surviving second value wins in the browser", got)
	}
}

func TestPreviewNoReferrerSurvivesTheHeaderWipeOnTheBootingPage(t *testing.T) {
	h := frameGuardFor(t, "pilldrops.com", 502)
	if got := h.Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("booting page Referrer-Policy = %q, want no-referrer — this response "+
			"replaces the node's header map wholesale", got)
	}
}

func TestPreviewRefusalPageSendsNoReferrer(t *testing.T) {
	s := NewServer("k", "t")
	s.sandboxDomain = ""
	req := httptest.NewRequest("GET", "/preview/sb1/8000/", nil) // no token
	req.SetPathValue("id", "sb1")
	req.SetPathValue("rest", "8000/")
	w := httptest.NewRecorder()
	if s.authorizePreview(w, req, "sb1", 8000) {
		t.Fatal("expected the request to be refused")
	}
	if got := w.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("refusal page Referrer-Policy = %q, want no-referrer", got)
	}
}

func TestFrameAncestorsPolicy(t *testing.T) {
	cases := []struct{ domain, want string }{
		{"", "frame-ancestors 'none'"},
		{"pilldrops.com", "frame-ancestors 'self' https://pilldrops.com https://*.pilldrops.com"},
	}
	for _, c := range cases {
		if got := frameAncestorsPolicy(c.domain, ""); got != c.want {
			t.Errorf("frameAncestorsPolicy(%q) = %q, want %q", c.domain, got, c.want)
		}
	}
}

// A deployment's portal may live on its own domain, and its Preview tab must
// still embed previews. A customer hostname is never embedded by anyone.
func TestFramePolicyAllowsTheDeploymentsPortal(t *testing.T) {
	got := frameAncestorsPolicy("sandboxes.example.com", "https://app.example.com")
	want := "frame-ancestors 'self' https://sandboxes.example.com https://*.sandboxes.example.com https://app.example.com"
	if got != want {
		t.Errorf("policy = %q, want %q", got, want)
	}
	if got := frameAncestorsPolicy("sandboxes.example.com", ""); strings.Contains(got, "  ") || strings.HasSuffix(got, " ") {
		t.Errorf("no portal origin left a gap in the policy: %q", got)
	}
	if got := frameAncestorsPolicy("", "https://app.example.com"); got != "frame-ancestors 'none'" {
		t.Errorf("customer host policy = %q, want framing denied", got)
	}
}

func TestPortalOriginGuard(t *testing.T) {
	for _, ok := range []string{"", "https://app.example.com", "https://app.example.com:8443", "http://localhost:3000"} {
		if err := PortalOriginGuardError(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"app.example.com", "https://", "https://app.example.com/path", "https://a.com https://evil.com", "https://a.com; script-src *", "*", "ftp://a.com", "http://app.example.com"} {
		if err := PortalOriginGuardError(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
