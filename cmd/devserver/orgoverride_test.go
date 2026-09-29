package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The portal holds the orchestrator's master key and asserts the tenant with
// X-Org-Override, which nodes trust. A client value surviving the hop would
// let it act as another customer.
func TestClientOrgOverrideNeverReachesTheOrchestrator(t *testing.T) {
	var seen string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("X-Org-Override")
	}))
	defer up.Close()

	for name, tc := range map[string]struct{ sent, owner, want string }{
		"forgery is discarded":        {"u:victim", "u:attacker", "u:attacker"},
		"forgery with no owner":       {"u:victim", "", ""},
		"legitimate owner passes":     {"", "u:real", "u:real"},
		"forgery cannot blank an org": {"", "u:real", "u:real"},
	} {
		seen = "<unset>"
		req := httptest.NewRequest("GET", "/v1/sandboxes/sb_x/deploy", nil)
		if tc.sent != "" {
			req.Header.Set("X-Org-Override", tc.sent)
		}
		proxyToOrchestrator(req, up.URL, "master-key", tc.owner)
		if seen == "<unset>" {
			t.Fatalf("%s: upstream was never called", name)
		}
		if seen != tc.want {
			t.Errorf("%s: client sent %q, portal asserted owner %q, upstream saw %q — want %q",
				name, tc.sent, tc.owner, seen, tc.want)
		}
	}
}

// The portal names the calling key beside the org it asserts, so a sandbox
// created through it belongs to whoever created it. A client's own X-Key-Id is
// its claim about itself and must never survive the hop.
func TestKeyIdIsDerivedFromTheCredentialNotRelayed(t *testing.T) {
	var org, keyID string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		org, keyID = r.Header.Get("X-Org-Override"), r.Header.Get("X-Key-Id")
	}))
	defer up.Close()

	req := httptest.NewRequest("POST", "/v1/sandboxes", nil)
	req.Header.Set("Authorization", "Bearer sfc_real")
	req.Header.Set("X-Key-Id", "k:forged")
	proxyToOrchestrator(req, up.URL, "master-key", "u:real")
	if org != "u:real" {
		t.Errorf("org = %q, want u:real", org)
	}
	if want := apiKeyOwner("sfc_real"); keyID != want {
		t.Errorf("X-Key-Id = %q, want %q derived from the caller's own token", keyID, want)
	}

	// No credential: nothing is asserted, and the client's claim still dies here.
	keyID = "<unset>"
	anon := httptest.NewRequest("POST", "/v1/sandboxes", nil)
	anon.Header.Set("X-Key-Id", "k:forged")
	proxyToOrchestrator(anon, up.URL, "master-key", "u:real")
	if keyID != "" {
		t.Errorf("X-Key-Id = %q for a credential-less request, want empty", keyID)
	}
}

// The header must not be reachable through the relay allowlist either.
func TestOrgOverrideIsNotInTheRelayAllowlist(t *testing.T) {
	var seen string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("X-Org-Override")
	}))
	defer up.Close()

	req := httptest.NewRequest("GET", "/v1/anything", nil)
	req.Header.Set("X-Org-Override", "u:victim")
	// owner empty: nothing should set the header at all.
	proxyToOrchestrator(req, up.URL, "master-key", "")
	if seen != "" {
		t.Errorf("upstream saw X-Org-Override=%q with no owner asserted", seen)
	}
}
