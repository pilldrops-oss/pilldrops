package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const ingressDomain = "pilldrops.com"

// mountIngress builds hostIngress over a recording handler standing in for the router.
func mountIngress(t *testing.T, domain string) (http.Handler, *ingressSink) {
	t.Helper()
	sink := &ingressSink{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sink.hits++
		sink.path = r.URL.Path
		sink.rawQuery = r.URL.RawQuery
		sink.host = r.Host
		_, _ = w.Write([]byte("next"))
	})
	s := NewServer("fleet-key", "nodetoken")
	s.sandboxDomain = domain
	return s.hostIngress(next), sink
}

type ingressSink struct {
	hits     int
	path     string
	rawQuery string
	host     string
}

func ingressReq(host, target string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "http://x"+target, nil)
	r.Host = host
	r.RemoteAddr = "203.0.113.7:1234"
	return r
}

// The subdomain form is rewritten into the path form handlePreview already serves.
func TestAppPreviewHostRewritesIntoThePathForm(t *testing.T) {
	h, sink := mountIngress(t, ingressDomain)

	h.ServeHTTP(httptest.NewRecorder(), ingressReq("8080-sb1."+ingressDomain, "/app/index.html?a=b"))

	if sink.hits != 1 {
		t.Fatalf("next hits = %d, want 1", sink.hits)
	}
	if want := "/preview/sb1/8080/app/index.html"; sink.path != want {
		t.Errorf("rewritten path = %q, want %q", sink.path, want)
	}
	// The query carries ?token= for the preview gate.
	if sink.rawQuery != "a=b" {
		t.Errorf("query = %q, want %q — the preview token rides in the query", sink.rawQuery, "a=b")
	}
}

// "agent-sb1" splits into ("agent", "sb1") and "agent" is not a port, so the
// agent origin must claim the request rather than let it fall through to the
// site router. With no sandbox placed, the claim shows up as the agent origin's 404.
func TestAgentHostIsNotEatenByThePortParse(t *testing.T) {
	h, sink := mountIngress(t, ingressDomain)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, ingressReq("agent-sb1."+ingressDomain, "/v1/agent/status"))

	if strings.HasPrefix(sink.path, "/preview/") {
		t.Errorf("agent host was rewritten to %q — the app-preview parse claimed it and the agent origin never ran", sink.path)
	}
	if sink.hits != 0 {
		t.Errorf("agent host fell through to the normal router (%d hits, path %q) — this is the defect where a share-link recipient was served the marketing site", sink.hits, sink.path)
	}
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 from the agent origin for an unplaced sandbox", w.Code)
	}
}

func TestEmptyDomainDisablesHostIngress(t *testing.T) {
	h, sink := mountIngress(t, "")

	h.ServeHTTP(httptest.NewRecorder(), ingressReq("8080-sb1.pilldrops.com", "/x"))

	if sink.hits != 1 || sink.path != "/x" {
		t.Errorf("with no domain configured: hits=%d path=%q, want 1 and %q untouched", sink.hits, sink.path, "/x")
	}
}

// The Host header is attacker-controlled and not validated as a DNS name
// upstream; a wrong /preview/ rewrite routes a request at another tenant.
func TestHostsThatMustNotBeReadAsAnAppPreview(t *testing.T) {
	for _, tc := range []struct {
		name string
		host string
		why  string
	}{
		{"apex", ingressDomain,
			"the domain itself is the dashboard, not a sandbox"},
		{"bare label, no port", "sb1." + ingressDomain,
			"no <port>- prefix: there is nothing to proxy to"},
		{"empty id", "8080-." + ingressDomain,
			"a port with no sandbox names no sandbox"},
		{"empty id, agent form", "agent-." + ingressDomain,
			"same, on the agent branch"},
		{"non-numeric port", "http-sb1." + ingressDomain,
			"the port must be a number or the target is unbounded"},
		{"port zero", "0-sb1." + ingressDomain,
			"port 0 is not a listening port"},
		{"port overflow", "99999-sb1." + ingressDomain,
			"above 65535 is not a port; must not wrap or truncate"},
		{"negative port", "-1-sb1." + ingressDomain,
			"splits to an empty port field, which is not a number"},
		{"deeper subdomain", "8080-sb1.evil." + ingressDomain,
			"only ONE label may precede the domain; a nested label is someone else's name"},
		// Matching on domain rather than "."+domain would accept these.
		{"suffix confusion, no dot", "8080-sb1.evil" + ingressDomain,
			"evilpilldrops.com is a DIFFERENT domain that ends in our string"},
		{"domain as a prefix", "8080-sb1." + ingressDomain + ".evil.com",
			"our domain appearing early does not make this our domain"},
		{"unrelated domain", "8080-sb1.example.org",
			"not our domain at all"},
		// The id is interpolated into the rewritten path.
		{"slash in id", "8080-a/b." + ingressDomain,
			"a slash in the id would restructure the rewritten /preview/ path"},
		{"traversal in id", "8080-../../v1/nodes." + ingressDomain,
			"dot-segments must never reach the path builder"},
		{"backslash in id", `8080-a\b.` + ingressDomain,
			"backslash is a separator on some parsers even when it is not on ours"},
		{"encoded traversal in id", "8080-%2e%2e%2fadmin." + ingressDomain,
			"the Host header is not percent-decoded, but the path builder's output may be"},
		{"null byte in id", "8080-sb1\x00." + ingressDomain,
			"a NUL can truncate the string for a downstream C consumer"},
		{"CR in id", "8080-sb1\r." + ingressDomain,
			"CRLF in a value that reaches a header or a log is injection"},
		{"space in id", "8080-sb 1." + ingressDomain,
			"whitespace is not a DNS label and must not be normalised away"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, sink := mountIngress(t, ingressDomain)
			h.ServeHTTP(httptest.NewRecorder(), ingressReq(tc.host, "/x"))
			if strings.HasPrefix(sink.path, "/preview/") {
				t.Errorf("Host %q was accepted as an app preview and rewritten to %q\nwhy this must not happen: %s",
					tc.host, sink.path, tc.why)
			}
		})
	}
}

func TestHostNormalisationThatMustHappen(t *testing.T) {
	for _, tc := range []struct {
		name, host, wantPath string
	}{
		{"explicit port is stripped",
			"8080-sb1." + ingressDomain + ":8443", "/preview/sb1/8080/x"},
		{"mixed case is lowered",
			"8080-SB1." + strings.ToUpper(ingressDomain), "/preview/sb1/8080/x"},
		{"lowest valid port",
			"1-sb1." + ingressDomain, "/preview/sb1/1/x"},
		{"highest valid port",
			"65535-sb1." + ingressDomain, "/preview/sb1/65535/x"},
		{"id may contain hyphens",
			"8080-sb-1-abc." + ingressDomain, "/preview/sb-1-abc/8080/x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, sink := mountIngress(t, ingressDomain)
			h.ServeHTTP(httptest.NewRecorder(), ingressReq(tc.host, "/x"))
			if sink.path != tc.wantPath {
				t.Errorf("Host %q rewrote to %q, want %q", tc.host, sink.path, tc.wantPath)
			}
		})
	}
}

// Without the trailing slash, relative asset URLs resolve against the wrong base.
func TestRootPathGetsASlash(t *testing.T) {
	h, sink := mountIngress(t, ingressDomain)
	r := ingressReq("8080-sb1."+ingressDomain, "/")
	r.URL.Path = "" // what "GET http://host HTTP/1.1" yields
	h.ServeHTTP(httptest.NewRecorder(), r)

	if want := "/preview/sb1/8080/"; sink.path != want {
		t.Errorf("empty path rewrote to %q, want %q", sink.path, want)
	}
}

func TestNonDomainHostFallsThroughUntouched(t *testing.T) {
	h, sink := mountIngress(t, ingressDomain)

	h.ServeHTTP(httptest.NewRecorder(), ingressReq("api.internal.example", "/v1/sandboxes?limit=3"))

	if sink.hits != 1 {
		t.Fatalf("next hits = %d, want 1", sink.hits)
	}
	if sink.path != "/v1/sandboxes" || sink.rawQuery != "limit=3" {
		t.Errorf("path/query = %q/%q, want /v1/sandboxes and limit=3 untouched", sink.path, sink.rawQuery)
	}
}
