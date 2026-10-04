package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

type stubResolver struct {
	target  string
	err     error
	gotID   string
	gotPort int
	gotOrg  string
}

func (s *stubResolver) PreviewTarget(_ context.Context, id string, port int, org string) (string, error) {
	s.gotID, s.gotPort, s.gotOrg = id, port, org
	return s.target, s.err
}

// gatedReq is httptest.NewRequest with the org the preview gate would have attached.
func gatedReq(method, target string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	return req.WithContext(WithPreviewOrg(req.Context(), "u:cust1"))
}

func TestPreviewResponsesCarryNoReferrer(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	h := Handler(&stubResolver{target: strings.TrimPrefix(upstream.URL, "http://")})
	req := gatedReq("GET", "/preview/sb_1/8000/?ptok=secret-credential")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("Referrer-Policy = %q, want no-referrer — the ?ptok= in this URL "+
			"would be forwarded to any third party the app links to or loads from", got)
	}
}

// ReverseProxy appends headers, so a tenant app's own policy must not leave two values.
func TestTenantAppCannotOverrideNoReferrer(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Referrer-Policy", "unsafe-url")
		io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	h := Handler(&stubResolver{target: strings.TrimPrefix(upstream.URL, "http://")})
	req := gatedReq("GET", "/preview/sb_1/8000/?ptok=secret-credential")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	got := rec.Header().Values("Referrer-Policy")
	if len(got) != 1 || got[0] != "no-referrer" {
		t.Fatalf("Referrer-Policy = %v, want exactly [no-referrer] — a second value "+
			"lets the sandbox app win and leak its own preview credential", got)
	}
}

func TestUnreachableAppStillCarriesNoReferrer(t *testing.T) {
	// 127.0.0.1:1 refuses immediately, taking the ErrorHandler path.
	h := Handler(&stubResolver{target: "127.0.0.1:1"})
	req := gatedReq("GET", "/preview/sb_1/8000/?ptok=secret-credential")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502 (the ErrorHandler path this test exists to cover)", rec.Code)
	}
	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("Referrer-Policy on the 502 = %q, want no-referrer", got)
	}
}

func TestProxyForwardsToTarget(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "app-saw:"+r.URL.Path)
	}))
	defer upstream.Close()
	target := strings.TrimPrefix(upstream.URL, "http://")

	res := &stubResolver{target: target}
	h := Handler(res)

	req := gatedReq("GET", "/preview/sb_1/8000/hello/world")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); body != "app-saw:/hello/world" {
		t.Fatalf("body = %q, want app-saw:/hello/world", body)
	}
	if res.gotID != "sb_1" || res.gotPort != 8000 {
		t.Fatalf("resolver got id=%q port=%d", res.gotID, res.gotPort)
	}
}

func TestProxyActionableErrorWhenAppDown(t *testing.T) {
	res := &stubResolver{target: "127.0.0.1:1"}
	h := Handler(res)

	req := gatedReq("GET", "/preview/sb_1/8000/")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "$PORT") {
		t.Fatalf("error body should mention the $PORT contract, got: %q", rec.Body.String())
	}
}

func TestProxyResolverErrors(t *testing.T) {
	res := &stubResolver{err: rt.ErrNotFound}
	h := Handler(res)
	req := gatedReq("GET", "/preview/nope/8000/")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", rec.Code)
	}
}

// An unpublished port is the caller's to fix (publish it at create), and a 404
// read as a wrong sandbox id (API I1.15).
func TestProxyNoAppOnPortReturns400(t *testing.T) {
	res := &stubResolver{err: rt.ErrNoAppOnPort}
	h := Handler(res)
	req := gatedReq("GET", "/preview/sb_1/9999/")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no app listening") {
		t.Fatalf("body = %q, want a no-app-on-port message", rec.Body.String())
	}
}

func TestProxyBadPaths(t *testing.T) {
	res := &stubResolver{target: "127.0.0.1:9"}
	h := Handler(res)
	for _, p := range []string{"/notpreview/x", "/preview/sb_1/", "/preview/sb_1/abc/", "/preview/sb_1/99999/", "/preview/sb_1/0/"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, gatedReq("GET", p))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("path %q -> %d, want 400", p, rec.Code)
		}
	}
}

func TestProxyResumeFailureIs503(t *testing.T) {
	res := &stubResolver{err: errors.New("restore: boom")}
	h := Handler(res)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, gatedReq("GET", "/preview/sb_1/8000/"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rec.Code)
	}
}

func TestForwardProxiesToTarget(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		fmt.Fprintf(w, "path=%s method=%s", r.URL.Path, r.Method)
	}))
	defer upstream.Close()
	target := strings.TrimPrefix(upstream.URL, "http://")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/functions/f/invoke/hi", nil)
	Forward(rec, req, target, "/hi", "upstream down")

	if rec.Code != 201 || rec.Body.String() != "path=/hi method=POST" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
}
