package mcp

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// apiStub stands in for the /v1 router and records the auth and paths it saw.
type apiStub struct {
	seenAuth  []string
	seenPaths []string
}

func (a *apiStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.seenAuth = append(a.seenAuth, r.Header.Get("Authorization"))
	a.seenPaths = append(a.seenPaths, r.URL.Path)
	if r.Header.Get("Authorization") == "" {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func post(t *testing.T, h http.Handler, body, auth string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestNonPostIsRejectedWithAllow(t *testing.T) {
	s := &HTTPServer{API: &apiStub{}}
	for _, m := range []string{http.MethodGet, http.MethodDelete, http.MethodPut} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(m, "/mcp", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s = %d, want 405", m, rec.Code)
		}
		if got := rec.Header().Get("Allow"); got != "POST" {
			t.Errorf("%s: Allow = %q, want POST", m, got)
		}
	}
}

func TestNotificationOverHTTPGets202AndNoBody(t *testing.T) {
	s := &HTTPServer{API: &apiStub{}}
	rec := post(t, s, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, "Bearer k")
	if rec.Code != http.StatusAccepted {
		t.Errorf("code = %d, want 202", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty for a notification", rec.Body.String())
	}
}

func TestDiscoveryIsOpen(t *testing.T) {
	api := &apiStub{}
	s := &HTTPServer{API: api}
	for _, method := range []string{"initialize", "tools/list", "ping"} {
		rec := post(t, s, `{"jsonrpc":"2.0","id":1,"method":"`+method+`"}`, "")
		if rec.Code != http.StatusOK {
			t.Errorf("%s unauthenticated = %d, want 200", method, rec.Code)
		}
	}
	if len(api.seenPaths) != 0 {
		t.Errorf("discovery reached the platform API: %v", api.seenPaths)
	}
}

func TestUnauthenticatedToolCallGets401WithOAuthPointer(t *testing.T) {
	s := &HTTPServer{API: &apiStub{}}
	rec := post(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_templates"}}`, "")

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
	wa := rec.Header().Get("WWW-Authenticate")
	if !strings.Contains(wa, "Bearer") || !strings.Contains(wa, "/.well-known/oauth-protected-resource") {
		t.Errorf("WWW-Authenticate = %q, want a Bearer challenge naming the resource metadata", wa)
	}
}

func TestChallengeOriginFollowsTheProxyHeaders(t *testing.T) {
	// TLS terminates upstream, so r.TLS is nil and the origin must come from the header.
	s := &HTTPServer{API: &apiStub{}}
	req := httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x"}}`))
	req.Host = "pilldrops.com"
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, "https://pilldrops.com/") {
		t.Errorf("WWW-Authenticate = %q, want the https origin from X-Forwarded-Proto", got)
	}
}

func TestAuthorizationReachesThePlatformAPI(t *testing.T) {
	api := &apiStub{}
	s := &HTTPServer{API: api}
	post(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_templates","arguments":{}}}`,
		"Bearer sk_live_123")

	if len(api.seenAuth) != 1 {
		t.Fatalf("platform API called %d times, want 1", len(api.seenAuth))
	}
	if api.seenAuth[0] != "Bearer sk_live_123" {
		t.Errorf("platform API saw Authorization %q, want it forwarded verbatim", api.seenAuth[0])
	}
}

// A batch the dispatcher runs but the auth gate misses skips the 401 that starts OAuth.
func TestAuthGateAgreesWithTheDispatcherOnBatches(t *testing.T) {
	api := &apiStub{}
	s := &HTTPServer{API: api}

	// First element is not an object.
	body := `[1,{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_templates","arguments":{}}}]`

	if !wantsToolCall([]byte(body)) {
		t.Errorf("wantsToolCall missed a tools/call the dispatcher does execute:\n  %s", body)
	}

	rec := post(t, s, body, "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated batch tool call = %d, want 401 so the client starts the OAuth flow", rec.Code)
	}
}

func TestWantsToolCallShapes(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want bool
	}{
		"single tool call":       {`{"jsonrpc":"2.0","id":1,"method":"tools/call"}`, true},
		"single list":            {`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, false},
		"single initialize":      {`{"jsonrpc":"2.0","id":1,"method":"initialize"}`, false},
		"batch with a tool call": {`[{"method":"tools/list"},{"method":"tools/call"}]`, true},
		"batch without one":      {`[{"method":"tools/list"},{"method":"ping"}]`, false},
		"empty batch":            {`[]`, false},
		"garbage":                {`not json`, false},
		"empty":                  {``, false},
	} {
		if got := wantsToolCall([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: wantsToolCall(%s) = %v, want %v", name, tc.body, got, tc.want)
		}
	}
}

func TestAuthenticatedToolCallReachesTheRightEndpoint(t *testing.T) {
	api := &apiStub{}
	s := &HTTPServer{API: api}
	rec := post(t, s,
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"get_sandbox","arguments":{"sandbox_id":"sb_42"}}}`,
		"Bearer k")

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if len(api.seenPaths) != 1 || api.seenPaths[0] != "/v1/sandboxes/sb_42" {
		t.Errorf("platform paths = %v, want [/v1/sandboxes/sb_42]", api.seenPaths)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("reply is not JSON: %v (%s)", err, rec.Body.String())
	}
	if resp["id"] != float64(9) {
		t.Errorf("reply id = %v, want 9", resp["id"])
	}
}

func TestResponsesAreJSONContentType(t *testing.T) {
	s := &HTTPServer{API: &apiStub{}}
	rec := post(t, s, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, "Bearer k")
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

func TestPublicOrigin(t *testing.T) {
	for name, tc := range map[string]struct {
		host, xfp string
		tls       bool
		want      string
	}{
		"plain http":         {"localhost:8080", "", false, "http://localhost:8080"},
		"direct tls":         {"pilldrops.com", "", true, "https://pilldrops.com"},
		"behind a tls proxy": {"pilldrops.com", "https", false, "https://pilldrops.com"},
		// The header wins even when it downgrades.
		"proxy says http": {"pilldrops.com", "http", true, "http://pilldrops.com"},
	} {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Host = tc.host
		if tc.tls {
			req.TLS = &tls.ConnectionState{}
		}
		if tc.xfp != "" {
			req.Header.Set("X-Forwarded-Proto", tc.xfp)
		}
		if got := publicOrigin(req); got != tc.want {
			t.Errorf("%s: publicOrigin = %q, want %q", name, got, tc.want)
		}
	}
}
