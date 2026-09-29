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

// bootingNode is a fake node whose app has not bound its port yet.
func bootingNode(t *testing.T, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte("App is not responding on port 8000.\n"))
	}))
}

func previewReq(t *testing.T, accept string) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()
	// A valid token so the private-by-default gate lets these through.
	tok := (&Server{apiKey: "k"}).previewToken(context.Background(), "sb1", 8000)
	req := httptest.NewRequest("GET", "/preview/sb1/8000/?token="+tok, nil)
	req.SetPathValue("id", "sb1")
	req.SetPathValue("rest", "8000/")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	return httptest.NewRecorder(), req
}

func TestPreviewRequiresToken(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("app"))
	}))
	defer node.Close()
	s := NewServer("k", "t")
	upsert(s.reg, "node-a", node.URL, "kk", "", nil, "", cluster.Capacity{}, time.Now())
	s.reg.Place("sb1", "node-a")

	noTok := httptest.NewRequest("GET", "/preview/sb1/8000/", nil)
	noTok.SetPathValue("id", "sb1")
	noTok.SetPathValue("rest", "8000/")
	w := httptest.NewRecorder()
	s.handlePreview(w, noTok)
	if w.Code != http.StatusForbidden {
		t.Fatalf("no-token preview = %d, want 403", w.Code)
	}

	tok := s.previewToken(context.Background(), "sb1", 8000)
	okReq := httptest.NewRequest("GET", "/preview/sb1/8000/?token="+tok, nil)
	okReq.SetPathValue("id", "sb1")
	okReq.SetPathValue("rest", "8000/")
	w2 := httptest.NewRecorder()
	s.handlePreview(w2, okReq)
	if w2.Code != http.StatusOK || !strings.Contains(w2.Body.String(), "app") {
		t.Fatalf("tokened preview = %d body=%q, want 200/app", w2.Code, w2.Body.String())
	}
}

func TestPreviewReadinessAgentGets503JSON(t *testing.T) {
	node := bootingNode(t, http.StatusBadGateway)
	defer node.Close()
	s := NewServer("k", "t")
	upsert(s.reg, "node-a", node.URL, "kk", "", nil, "", cluster.Capacity{}, time.Now())
	s.reg.Place("sb1", "node-a")

	w, req := previewReq(t, "application/json")
	s.handlePreview(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("agent readiness status = %d, want 503 (D-9: bare 200 lies to pollers)", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Errorf("missing Retry-After on 503")
	}
	body := w.Body.String()
	if !strings.Contains(body, `"state"`) || !strings.Contains(body, "starting") {
		t.Errorf("agent body = %q, want JSON with state:starting", body)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("agent Content-Type = %q, want application/json", ct)
	}
}

// Accept: */* or empty is treated as an agent, not a browser.
func TestPreviewReadinessDefaultIsJSON(t *testing.T) {
	node := bootingNode(t, http.StatusServiceUnavailable)
	defer node.Close()
	s := NewServer("k", "t")
	upsert(s.reg, "node-a", node.URL, "kk", "", nil, "", cluster.Capacity{}, time.Now())
	s.reg.Place("sb1", "node-a")

	w, req := previewReq(t, "") // no Accept header
	s.handlePreview(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("default readiness status = %d, want 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "starting") {
		t.Errorf("default body = %q, want JSON", w.Body.String())
	}
}

func TestPreviewReadinessBrowserGetsHTMLAt503(t *testing.T) {
	node := bootingNode(t, http.StatusBadGateway)
	defer node.Close()
	s := NewServer("k", "t")
	upsert(s.reg, "node-a", node.URL, "kk", "", nil, "", cluster.Capacity{}, time.Now())
	s.reg.Place("sb1", "node-a")

	w, req := previewReq(t, "text/html,application/xhtml+xml")
	s.handlePreview(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("browser readiness status = %d, want 503", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Errorf("missing Retry-After for browser")
	}
	if !strings.Contains(w.Body.String(), "Starting your app") {
		t.Errorf("browser body should be the friendly HTML page, got %q", w.Body.String()[:min(80, len(w.Body.String()))])
	}
}

func TestPreviewLiveAppPassesThrough(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello from the app"))
	}))
	defer node.Close()
	s := NewServer("k", "t")
	upsert(s.reg, "node-a", node.URL, "kk", "", nil, "", cluster.Capacity{}, time.Now())
	s.reg.Place("sb1", "node-a")

	w, req := previewReq(t, "application/json")
	s.handlePreview(w, req)

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "hello from the app") {
		t.Fatalf("live app response was altered: status=%d body=%q", w.Code, w.Body.String())
	}
}
