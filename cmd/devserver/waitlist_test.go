package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWaitlistHandlerHoneypotDropsSilently: a filled honeypot pretends success
// and never reaches the orchestrator.
func TestWaitlistHandlerHoneypotDropsSilently(t *testing.T) {
	var hit bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	h := newWaitlistHandler(upstream.URL, "k", "")
	req := httptest.NewRequest("POST", "/waitlist", strings.NewReader(`{"email":"a@b.com","website":"http://spam.example"}`))
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (pretend success)", w.Code)
	}
	if hit {
		t.Fatalf("honeypot filled but the upstream orchestrator was still called")
	}
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	if out["ok"] != true {
		t.Fatalf("body = %s, want ok:true", w.Body.String())
	}
}

// TestWaitlistHandlerInvalidEmail400 rejects the email before contacting the upstream.
func TestWaitlistHandlerInvalidEmail400(t *testing.T) {
	var hit bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	h := newWaitlistHandler(upstream.URL, "k", "")

	for _, body := range []string{`{"email":""}`, `{"email":"not-an-email"}`, `{}`} {
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest("POST", "/waitlist", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("body=%s status = %d, want 400", body, w.Code)
		}
	}
	if hit {
		t.Fatalf("invalid email must not reach the upstream")
	}
}

// TestWaitlistHandlerForwardsToOrchestrator: the client sees ok only once the
// synchronous forward succeeds.
func TestWaitlistHandlerForwardsToOrchestrator(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	h := newWaitlistHandler(upstream.URL, "sekret", "")
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest("POST", "/waitlist", strings.NewReader(`{"email":"A@B.com","source":"pricing"}`)))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if gotPath != "/v1/waitlist" {
		t.Fatalf("upstream path = %q, want /v1/waitlist", gotPath)
	}
	if gotAuth != "Bearer sekret" {
		t.Fatalf("upstream auth = %q, want Bearer sekret", gotAuth)
	}
	if gotBody["email"] != "a@b.com" || gotBody["source"] != "pricing" {
		t.Fatalf("forwarded body = %+v, want normalized email + source", gotBody)
	}
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	if out["ok"] != true {
		t.Fatalf("response body = %s, want ok:true", w.Body.String())
	}
}

// TestWaitlistHandlerUpstreamDown: an unreachable orchestrator is a clean
// non-500 error.
func TestWaitlistHandlerUpstreamDown(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := upstream.URL
	upstream.Close() // now unreachable

	h := newWaitlistHandler(deadURL, "k", "")
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest("POST", "/waitlist", strings.NewReader(`{"email":"a@b.com"}`)))

	if w.Code == http.StatusOK || w.Code == http.StatusInternalServerError {
		t.Fatalf("status = %d, want a clean non-200 non-500 error", w.Code)
	}
	var out map[string]any
	if json.Unmarshal(w.Body.Bytes(), &out) != nil || out["error"] == nil {
		t.Fatalf("body = %s, want a JSON error envelope", w.Body.String())
	}
}

// TestWaitlistHandlerNoOrchestratorConfigured: unset orchestrator env must not
// 500 the marketing page.
func TestWaitlistHandlerNoOrchestratorConfigured(t *testing.T) {
	h := newWaitlistHandler("", "", "")
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest("POST", "/waitlist", strings.NewReader(`{"email":"a@b.com"}`)))
	if w.Code == http.StatusInternalServerError {
		t.Fatalf("status = %d, must not be a 500", w.Code)
	}
	if w.Code == http.StatusOK {
		t.Fatalf("status = 200 but nothing durable happened — must not claim success")
	}
}
