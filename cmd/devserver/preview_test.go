package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPreviewReverseProxyForwardsToOrchestrator: /preview/{id}/{port}/... is
// forwarded with the full path preserved.
func TestPreviewReverseProxyForwardsToOrchestrator(t *testing.T) {
	var gotPath string
	orch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("X-From", "orchestrator")
		_, _ = io.WriteString(w, "app response body")
	}))
	defer orch.Close()

	h := previewReverseProxy(orch.URL)
	if h == nil {
		t.Fatal("previewReverseProxy returned nil for a valid orchestrator URL")
	}

	req := httptest.NewRequest("GET", "/preview/sb-123/5173/deep/path?q=1", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if gotPath != "/preview/sb-123/5173/deep/path" {
		t.Fatalf("orchestrator saw path %q, want the full preview path preserved", gotPath)
	}
	if w.Header().Get("X-From") != "orchestrator" || w.Body.String() != "app response body" {
		t.Fatalf("response not relayed from orchestrator: hdr=%q body=%q", w.Header().Get("X-From"), w.Body.String())
	}
}

// TestPreviewReverseProxyBadURL: an unparseable orchestrator URL yields no
// handler rather than a panic.
func TestPreviewReverseProxyBadURL(t *testing.T) {
	if previewReverseProxy("://not-a-url") != nil {
		t.Fatal("expected nil handler for an unparseable orchestrator URL")
	}
	if previewReverseProxy("") != nil {
		t.Fatal("expected nil handler for an empty orchestrator URL")
	}
}
