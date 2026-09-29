package orchestrator

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// desktopMark is what the fake node serves on the desktop port, so "the link
// opened" means the guest answered rather than some 200-status error page.
const desktopMark = "DESKTOP-ANSWERED"

// nodeExtraURLFixture is a node that answers create and get with the link set a
// real node builds: url, terminal_url and desktop_url, each on the node's own
// host with the node's own hop ?ptok= (internal/httpapi appURL).
func nodeExtraURLFixture(t *testing.T) *Server {
	t.Helper()
	const body = `{"id":"sb-1","template":"base","state":"running","has_desktop":true,` +
		`"url":"http://node.invalid/preview/sb-1/8000/?ptok=the-node-hop-credential",` +
		`"terminal_url":"http://node.invalid/preview/sb-1/7681/?ptok=the-node-hop-credential",` +
		`"desktop_url":"http://node.invalid/preview/sb-1/6080/?ptok=the-node-hop-credential"}`
	s, _ := createFailureFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes",
			r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sb-1":
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodPost {
				w.WriteHeader(http.StatusCreated)
			}
			_, _ = io.WriteString(w, body)
		case strings.HasPrefix(r.URL.Path, "/preview/sb-1/6080"):
			_, _ = io.WriteString(w, desktopMark)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	return s
}

// desktop_url was advertised on every create and relayed verbatim: a node-host
// URL a caller cannot open, carrying the hop credential previewLinks states as
// an invariant must never travel to a caller. terminal_url had the same shape.
// The positive control is the one that matters — the link must actually open
// the desktop through this tier, which the node-host value never did.
func TestEveryNodeMintedLinkIsRewrittenForTheCaller(t *testing.T) {
	s := nodeExtraURLFixture(t)
	w := doCreate(t, s, `{"template":"base","vcpu":1,"mem_mb":512}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body %s", w.Code, w.Body.String())
	}
	assertNoNodeCredential(t, "create", w.Body.Bytes())

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	desktop, _ := body["desktop_url"].(string)
	if desktop == "" {
		t.Fatal("no desktop_url in the create response")
	}
	code, got := open(t, s, desktop)
	if code != http.StatusOK || !strings.Contains(got, desktopMark) {
		t.Fatalf("desktop_url does not open the desktop: GET %s = %d %.120q", desktop, code, got)
	}
	// Negative control: without its credential the same link is refused, so the
	// pass above is not just an open gate.
	bare, _, _ := strings.Cut(desktop, "?")
	if code, got := open(t, s, bare); code == http.StatusOK && strings.Contains(got, desktopMark) {
		t.Fatalf("the same link with no credential also opened (%d): the gate is not on", code)
	}
}

// The same relay on the read path: GET used to hand the node's body straight
// back, credential and unreachable host included.
func TestSandboxGetDoesNotRelayTheNodeCredential(t *testing.T) {
	s := nodeExtraURLFixture(t)
	if w := doCreate(t, s, `{"template":"base","vcpu":1,"mem_mb":512}`); w.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body %s", w.Code, w.Body.String())
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/sb-1", nil)
	req.Header.Set("Authorization", "Bearer api-key")
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get status = %d, body %s", w.Code, w.Body.String())
	}
	assertNoNodeCredential(t, "get", w.Body.Bytes())

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	// The CLI's open verb reads viewer_url before desktop_url, and this tier
	// has never minted a viewer_url on a GET. Minting one here would send
	// `sandbox open` on a desktop sandbox back to the app port.
	if v, ok := body["viewer_url"]; ok {
		t.Errorf("GET now carries viewer_url (%v); a client that prefers it never reaches desktop_url", v)
	}
	desktop, _ := body["desktop_url"].(string)
	if !strings.Contains(desktop, "token=") {
		t.Errorf("desktop_url on a GET carries no credential, so it opens nothing on a private sandbox: %q", desktop)
	}
	if code, got := open(t, s, desktop); code != http.StatusOK || !strings.Contains(got, desktopMark) {
		t.Fatalf("desktop_url from a GET does not open the desktop: %d %.120q", code, got)
	}
}

func assertNoNodeCredential(t *testing.T, what string, body []byte) {
	t.Helper()
	if strings.Contains(string(body), "ptok=") {
		t.Errorf("the %s response relays the node's own ?ptok= to the caller: %s", what, body)
	}
	if strings.Contains(string(body), "node.invalid") {
		t.Errorf("the %s response hands back a node-host URL the caller cannot open: %s", what, body)
	}
}
