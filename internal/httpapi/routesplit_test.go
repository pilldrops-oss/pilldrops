package httpapi

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// These tests pin the route table so moving a block between registrar files
// cannot silently drop or duplicate a route.

var routeCallRe = regexp.MustCompile(`route\("([^"]+)"`)

func registeredRoutes(t *testing.T) []string {
	t.Helper()
	var out []string
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		body, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range routeCallRe.FindAllStringSubmatch(string(body), -1) {
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}

// TestEveryRouteSurvivedTheSplit pins the total declared in source, ignoring
// build tags. Exact rather than a floor so an added route forces a decision on where it goes.
func TestEveryRouteSurvivedTheSplit(t *testing.T) {
	const want = 86 // -8: functions moved to the cloud product
	got := registeredRoutes(t)
	if len(got) != want {
		t.Errorf("the node registers %d routes, expected %d.\n\n"+
			"If you ADDED one: put it in the registrar for its feature area, then bump this "+
			"number.\nIf you MOVED one and this dropped: the move lost a route, which 404s in "+
			"production while every test for its handler still passes.", len(got), want)
	}
}

// TestNoRouteIsRegisteredTwice guards the ServeMux duplicate-pattern panic,
// which fires at boot rather than at build.
func TestNoRouteIsRegisteredTwice(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range registeredRoutes(t) {
		if seen[r] {
			t.Errorf("route %q is registered twice — ServeMux panics on this at boot, not at build", r)
		}
		seen[r] = true
	}
}

// rawPatterns are mounted without s.instrument and so are absent from registeredRoutes.
var rawPatterns = []string{
	"/preview/",
}

// concretize turns a ServeMux pattern into a request path that matches it. The
// placeholder must not appear as a literal segment anywhere in the route table.
func concretize(pattern string) string {
	out := wildcardRestRe.ReplaceAllString(pattern, "zz/zz")
	return wildcardRe.ReplaceAllString(out, "zz")
}

var (
	wildcardRestRe = regexp.MustCompile(`\{[^}]+\.\.\.\}`)
	wildcardRe     = regexp.MustCompile(`\{[^}]+\}`)
)

// newRouteMux builds the real mux. New(nil, nil) is enough because Register
// never calls a handler.
func newRouteMux(t *testing.T) *http.ServeMux {
	t.Helper()
	return New(nil, nil).Routes()
}

// mountedRoutes returns the declared and raw patterns the live mux actually
// resolves. Only the core-tagged test file calls it, which the linter cannot see.
//
//nolint:unused // referenced from the core-tagged test file
func mountedRoutes(t *testing.T) []string {
	t.Helper()
	mux := newRouteMux(t)
	var out []string
	for _, want := range append(registeredRoutes(t), rawPatterns...) {
		method, path := "GET", want
		if m, p, ok := strings.Cut(want, " "); ok {
			method, path = m, p
		}
		if _, got := mux.Handler(httptest.NewRequest(method, concretize(path), nil)); got == want {
			out = append(out, want)
		}
	}
	return out
}

// routesInFile returns the route patterns registered by one file.
func routesInFile(t *testing.T, name string) []string {
	t.Helper()
	body, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range routeCallRe.FindAllStringSubmatch(string(body), -1) {
		out = append(out, m[1])
	}
	return out
}

// featureRegistrars is every per-feature route module; server.go is the core.
var featureRegistrars = []string{
	"imageroutes.go",
	"opsroutes.go",
	"cronroutes.go",
	"checkpoints.go",
}

// TestCoreRouteCountIsPinned stops the core growing back; a route added to
// server.go instead of its registrar leaves the total unchanged, so only this count notices.
func TestCoreRouteCountIsPinned(t *testing.T) {
	const wantCore = 51 // -1: channels, which had no bridge left to report on
	got := routesInFile(t, "server.go")
	if len(got) != wantCore {
		t.Errorf("server.go registers %d routes, expected %d.\n\n"+
			"A route added to Register rather than to its feature's registrar makes the "+
			"core bigger without changing the total, so no other test here fails.\n"+
			"Move it to one of %v, or justify the growth and bump this number.",
			len(got), wantCore, featureRegistrars)
	}
	for _, r := range got {
		if strings.Contains(r, "/agent") || strings.Contains(r, "/cron") ||
			strings.Contains(r, "/functions") || strings.Contains(r, "/integrations") ||
			strings.Contains(r, "/images") {
			t.Errorf("route %q is registered in server.go but belongs to an extracted "+
				"feature area", r)
		}
	}
}

// TestEveryFeatureRegistrarIsNonEmpty guards against the extraction regex
// drifting and every count here passing on an empty set.
func TestEveryFeatureRegistrarIsNonEmpty(t *testing.T) {
	for _, f := range featureRegistrars {
		if n := len(routesInFile(t, f)); n == 0 {
			t.Fatalf("%s registers nothing — either it was gutted or the extraction "+
				"regex has drifted, and this whole file is then passing on an empty set", f)
		}
	}
}

// rawCallRe matches the raw helper, which mounts without s.instrument.
var rawCallRe = regexp.MustCompile(`raw\("([^"]+)"`)

// TestRawRegistrationsArePinned pins the raw set exactly, since those patterns
// are invisible to registeredRoutes and skip the request middleware.
func TestRawRegistrationsArePinned(t *testing.T) {
	want := map[string]bool{
		"/preview/": true,
		// The L4 port-forward tunnel: the connection is hijacked and spliced, so
		// a body cap would cap the customer's own stream and a server span would
		// stay open for the life of a database session. Gated by the preview
		// token and throttled per IP instead.
		"GET /v1/sandboxes/{id}/tcp/{port}": true,
		"GET /v1/sandboxes/{id}/udp/{port}": true,
	}
	got := map[string]bool{}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		for _, m := range rawCallRe.FindAllStringSubmatch(string(mustRead(t, e.Name())), -1) {
			got[m[1]] = true
		}
	}
	if len(got) == 0 {
		t.Fatal("no raw registrations found at all — the helper was renamed and this " +
			"guard is passing on an empty set")
	}
	for p := range want {
		if !got[p] {
			t.Errorf("raw pattern %q is gone. It bypasses s.instrument on purpose; if it "+
				"moved to the instrumented helper it now carries a %d-byte body cap it "+
				"did not have before", p, 64<<20)
		}
	}
	for p := range got {
		if !want[p] {
			t.Errorf("raw pattern %q is new. Mounting outside s.instrument skips the "+
				"request-body cap, the request-id header and the server span — say why "+
				"here rather than adding it silently", p)
		}
	}
}

func mustRead(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
