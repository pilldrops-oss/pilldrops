package httpapi

import (
	"net/http"
	"testing"
)

// Address and CIDR rules are enforced in the kernel chain of the sandbox's
// netns; a domain needs the name layer that reads SNI/Host, and there isn't one
// yet. The failure this guards is a 201 for a domain rule: the caller asked for
// a confined sandbox, got one that reaches the whole internet, and cannot tell.
func TestCreateRefusesDomainEgressRulesUntilTheNameLayerExists(t *testing.T) {
	srv, key := newTestServer(t)
	for _, body := range []string{
		`{"template":"base","network":{"allow":["api.stripe.com"]}}`,
		`{"template":"base","network":{"allow":["*.github.com"]}}`,
		`{"template":"base","network":{"deny":["evil.example.com"]}}`,
	} {
		code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, body)
		if code != http.StatusNotImplemented {
			t.Errorf("create %s returned %d (%v), want 501 — a domain rule nothing evaluates reads as protection", body, code, out)
		}
		if out["code"] != codeEgressUnenforced {
			t.Errorf("create %s answered code %v, want %q so a client can tell this apart from a bad request", body, out["code"], codeEgressUnenforced)
		}
	}

	// A create without rules is untouched.
	if code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base"}`); code != 201 {
		t.Fatalf("a plain create returned %d (%v), want 201 — the refusal caught a request that carried no rules", code, out)
	}
}

// An address rule reaches a runtime that cannot enforce it — this test server's
// fake, like any docker node — and that boot must fail rather than come up open.
func TestCreateRefusesAddressRulesOnARuntimeThatCannotEnforceThem(t *testing.T) {
	srv, key := newTestServer(t)
	for _, body := range []string{
		`{"template":"base","network":{"allow":["203.0.113.7"]}}`,
		`{"template":"base","network":{"deny":["0.0.0.0/0"]}}`,
		`{"template":"base","network":{}}`, // a policy permitting nothing: the strictest ask of all
	} {
		code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, body)
		if code == 201 {
			t.Errorf("create %s booted a sandbox on a runtime with no egress enforcement (%v); "+
				"the caller believes it is confined", body, out)
			continue
		}
		if out["code"] != codeEgressUnenforced {
			t.Errorf("create %s answered %d/%v, want the egress code so the caller knows to place it elsewhere", body, code, out["code"])
		}
	}
}
