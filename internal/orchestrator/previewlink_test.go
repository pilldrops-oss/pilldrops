package orchestrator

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// guestMark is what the fake node's preview serves, so "the link opened" means
// the guest answered and not that some error page came back with a 200.
const guestMark = "GUEST-ANSWERED"

// nodeCreateFixture is a node that answers a create with the URL a real node
// returns — its own ?ptok=, minted with the HMAC secret — and serves its
// preview mount to whoever the orchestrator lets through.
func nodeCreateFixture(t *testing.T) *Server {
	t.Helper()
	s, _ := createFailureFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"sb-1","template":"base","state":"running",`+
				`"url":"http://node.invalid/preview/sb-1/8000/?ptok=the-node-hop-credential"}`)
		case strings.HasPrefix(r.URL.Path, "/preview/sb-1/8000"):
			_, _ = io.WriteString(w, guestMark)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	return s
}

// createLink returns the link a caller is told to open, the way every client
// picks it (viewer_url, then app_url, then url), plus the raw body.
func createLink(t *testing.T, s *Server) (link string, body map[string]any) {
	t.Helper()
	w := doCreate(t, s, `{"template":"base","vcpu":1,"mem_mb":512}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	for _, k := range []string{"viewer_url", "app_url", "url"} {
		if v, _ := body[k].(string); v != "" {
			return v, body
		}
	}
	t.Fatalf("the create response carries no preview link at all: %s", w.Body.String())
	return "", nil
}

// open fetches a link through the orchestrator's own routes.
func open(t *testing.T, s *Server, link string) (int, string) {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatalf("unparseable link %q: %v", link, err)
	}
	req := httptest.NewRequest(http.MethodGet, u.RequestURI(), nil)
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// The link a create response hands back has to be one this tier's own gate
// opens. Each half was covered before and each passed alone — the node minted
// a token its own verifier accepted, the gate accepted tokens a test minted
// for it — and between them every create returned a URL that answered 403.
// This is the round trip, so a future change to either secret fails here.
func TestTheLinkCreateReturnsIsOneThisTierOpens(t *testing.T) {
	s := nodeCreateFixture(t)
	link, body := createLink(t, s)

	if code, got := open(t, s, link); code != http.StatusOK || !strings.Contains(got, guestMark) {
		t.Fatalf("the link the create response returned does not open: GET %s = %d %.120q",
			link, code, got)
	}
	// The negative control: the same link without its credential must be
	// refused, or the pass above says only that the gate is open.
	bare, _, _ := strings.Cut(link, "?")
	if code, got := open(t, s, bare); code == http.StatusOK && strings.Contains(got, guestMark) {
		t.Fatalf("the same link with no credential also opened (%d): the gate is not on", code)
	}
	// The node hop is the control plane's credential for the node, not the
	// caller's for the fleet. Handing it out is both a link that cannot open
	// on a fleet node and a credential given to the wrong audience.
	for _, k := range []string{"url", "app_url", "viewer_url"} {
		if v, _ := body[k].(string); strings.Contains(v, "ptok=") {
			t.Errorf("%s relays the node's own ?ptok= to the caller: %s", k, v)
		}
	}
}

// url/app_url stay bare so they can be logged, and viewer_url is the one that
// carries the token — the same split the portal makes and the refusal page names.
func TestCreateSeparatesTheBareURLFromTheOneThatOpens(t *testing.T) {
	s := nodeCreateFixture(t)
	_, body := createLink(t, s)

	viewer, _ := body["viewer_url"].(string)
	plain, _ := body["url"].(string)
	if viewer == "" {
		t.Fatal("no viewer_url in the create response: the refusal page tells people to open one")
	}
	if !strings.Contains(viewer, "token=") {
		t.Errorf("viewer_url carries no credential: %s", viewer)
	}
	if strings.Contains(plain, "token=") {
		t.Errorf("url carries a credential; the token belongs on viewer_url only: %s", plain)
	}
	if !strings.HasPrefix(viewer, plain) {
		t.Errorf("viewer_url %q is not url %q plus its token", viewer, plain)
	}
}
