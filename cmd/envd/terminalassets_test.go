package main

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// Re-vendoring a file without updating the manifest fails here.
func TestEmbeddedAssetsMatchTheirPinnedSRI(t *testing.T) {
	if len(terminalAssets) != 4 {
		t.Fatalf("manifest has %d entries, expected the 4 the page loads", len(terminalAssets))
	}
	for _, a := range terminalAssets {
		got, b, ok := terminalAssetBytes(a.Name)
		if !ok {
			t.Errorf("%s: not embedded", a.Name)
			continue
		}
		if len(b) == 0 {
			t.Errorf("%s: embedded but empty", a.Name)
			continue
		}
		if sum := sriOf(b); sum != got.SRI {
			t.Errorf("%s (%s): embedded bytes are %s, manifest pins %s",
				a.Name, a.Origin, sum, got.SRI)
		}
	}
}

func TestTerminalPageMakesNoOffOriginRequest(t *testing.T) {
	rec := httptest.NewRecorder()
	handleTerminalPage(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()

	for _, pat := range []string{`https://`, `http://`, `src="//`, `href="//`} {
		if strings.Contains(body, pat) {
			t.Errorf("terminal page contains %q — it must load nothing off-origin", pat)
		}
	}

	if strings.Contains(body, "jsdelivr") {
		t.Error("terminal page still references jsdelivr")
	}
}

func TestTerminalPageServesEveryAssetItReferences(t *testing.T) {
	rec := httptest.NewRecorder()
	handleTerminalPage(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()

	// The bootstrap names its assets as quoted basenames.
	re := regexp.MustCompile(`'([a-z-]+\.(?:js|css))'`)
	found := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		found[m[1]] = true
	}
	if len(found) == 0 {
		t.Fatal("found no asset references in the page — the matcher is broken, not the page")
	}

	srv := httptest.NewServer(terminalMux(newHashStore("deadbeef")))
	defer srv.Close()

	for name := range found {
		resp, err := http.Get(srv.URL + "/" + name)
		if err != nil {
			t.Fatalf("GET %s: %v", name, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("page references %s but the server answers %d", name, resp.StatusCode)
		}
	}
	for _, a := range terminalAssets {
		if !found[a.Name] {
			t.Errorf("manifest ships %s but the page never loads it", a.Name)
		}
	}
}

// A relative src under a preview URL with no trailing slash arrives with leftover path before the filename.
func TestAssetsResolveUnderAPathPrefixedPreview(t *testing.T) {
	srv := httptest.NewServer(terminalMux(newHashStore("deadbeef")))
	defer srv.Close()

	for _, p := range []string{
		"/xterm.js",
		"/preview/sb-1/8000/xterm.js",
		"/preview/sb-1/xterm.js",
	} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", p, resp.StatusCode)
		}
	}

	// Only manifest names may reach the embedded FS.
	for _, p := range []string{"/assets/xterm.js.map", "/../go.mod", "/xterm.jsx"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("GET %s = 200; only the four manifest entries may be served", p)
		}
	}
}

// A CSP hash that does not match the shipped script renders a black page with the reason only in the console.
func TestTerminalCSPAuthorisesExactlyThePageInlineScript(t *testing.T) {
	rec := httptest.NewRecorder()
	handleTerminalPage(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()

	csp := rec.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("no Content-Security-Policy header on the terminal page")
	}
	if csp != terminalCSP(terminalBootstrapJS) {
		t.Error("the served CSP is not the one computed from the page's inline script")
	}
	if !strings.Contains(body, csp) {
		t.Error("the <meta> CSP and the header CSP differ")
	}

	if !strings.Contains(body, "<script>"+terminalBootstrapJS+"</script>") {
		t.Error("the page's inline script is not byte-identical to the string the CSP hashed")
	}

	scriptSrc := ""
	for _, d := range strings.Split(csp, "; ") {
		if strings.HasPrefix(d, "script-src ") {
			scriptSrc = d
		}
	}
	if scriptSrc == "" {
		t.Fatal("CSP has no script-src")
	}
	if strings.Contains(scriptSrc, "'unsafe-inline'") {
		t.Errorf("script-src carries 'unsafe-inline', which nullifies the hash: %q", scriptSrc)
	}
	if !strings.Contains(scriptSrc, "'sha256-") {
		t.Errorf("script-src carries no hash: %q", scriptSrc)
	}
	if !strings.HasPrefix(csp, "default-src 'none'") {
		t.Errorf("CSP does not start from a deny-all default: %q", csp)
	}
}
