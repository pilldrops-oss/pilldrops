package orchestrator

import (
	"net/http"
	"testing"
)

// The control plane must refuse a domain rule itself, not lean on the node: a
// node running an older binary accepts the field and answers 201, and the caller
// is handed a sandbox that reads as confined and reaches anything. Address and
// CIDR rules pass through — the node's netns chain enforces those.
//
// No node is registered here, so a create that reached scheduling would 503 —
// a 501 also proves the refusal happens before placement.
func TestControlPlaneRefusesDomainEgressRulesUntilTheNameLayerExists(t *testing.T) {
	srv, _ := newProjectServer(t)

	for _, body := range []string{
		`{"template":"base","network":{"allow":["api.stripe.com"]}}`,
		`{"template":"base","network":{"deny":["*.evil.example"]}}`,
	} {
		code, out := do(t, srv, "POST", "/v1/sandboxes", "fleet-key", body)
		if code != http.StatusNotImplemented {
			t.Errorf("create %s = %d (%v), want 501 — an unenforced allowlist reads as protection", body, code, out)
		}
		if out["code"] != "egress_not_enforced" {
			t.Errorf("create %s answered code %v, want the same code the node uses", body, out["code"])
		}
	}

	// An address-only policy is enforceable, so the control plane forwards it;
	// with no node registered that is a 503, never the 501 above.
	for _, body := range []string{
		`{"template":"base","network":{"allow":["203.0.113.7"]}}`,
		`{"template":"base","network":{"deny":["0.0.0.0/0"]}}`,
		`{"template":"base","network":null}`,
	} {
		if code, out := do(t, srv, "POST", "/v1/sandboxes", "fleet-key", body); code == http.StatusNotImplemented {
			t.Errorf("create %s was refused as unenforceable (%v) — address rules are enforced "+
				"in the node's netns chain, so this refuses a policy that works", body, out)
		}
	}
}
