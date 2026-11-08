package orchestrator

import (
	"net/http"
	"strings"
	"testing"
)

// previewCookie opens a token link to private sb1 on host and returns the
// access cookie it was granted.
func previewCookie(t *testing.T, s *Server, host string) string {
	t.Helper()
	if err := s.st.Load().SetPublicPreview(t.Context(), "sb1", false); err != nil {
		t.Fatalf("make sb1 private: %v", err)
	}
	tok := s.previewToken(t.Context(), "sb1", 8000)

	w := hostReq(t, s, host, "/?token="+tok)
	if w.Code != http.StatusOK {
		t.Fatalf("token link = %d %q", w.Code, w.Body.String())
	}
	for _, c := range w.Header().Values("Set-Cookie") {
		if strings.HasPrefix(c, "pdpv_sb1_8000=") {
			return c
		}
	}
	t.Fatal("no preview cookie granted")
	return ""
}

// The dashboard embeds previews from another site, so the access cookie a
// ?token= grants must be one a browser keeps inside a third-party iframe.
func TestPreviewCookieWorksInsideACrossSiteFrame(t *testing.T) {
	ck := previewCookie(t, legacyServer(t), "8000-sb1.pilldrops.app")
	for _, attr := range []string{"SameSite=None", "Secure", "Partitioned", "HttpOnly"} {
		if !strings.Contains(ck, attr) {
			t.Errorf("preview cookie %q lacks %s", ck, attr)
		}
	}
}

// With the dashboard on the preview domain nothing embeds cross-site, so the
// cookie keeps the attributes it always had.
func TestPreviewCookieStaysLaxWithTheDashboardOnThePreviewDomain(t *testing.T) {
	s, _ := domainServer(t)
	ck := previewCookie(t, s, "8000-sb1.pilldrops.com")
	if !strings.Contains(ck, "SameSite=Lax") || strings.Contains(ck, "Partitioned") ||
		!strings.Contains(ck, "Secure") || !strings.Contains(ck, "HttpOnly") {
		t.Fatalf("preview cookie %q, want Secure, HttpOnly, SameSite=Lax and not Partitioned", ck)
	}
}
