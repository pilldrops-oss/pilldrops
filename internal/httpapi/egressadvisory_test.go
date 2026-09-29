package httpapi

import (
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/runtime/runtimetest"
)

// A deny-only policy is legal, applied, and means no egress whatsoever: the
// allow list it narrows is empty, so the chain ends in a bare drop. Resolve has
// always known this and nothing read the answer, so the caller got a 201 and a
// sandbox with no network. The create response must say so.
func TestCreateReportsTheAdvisoryThatADenyOnlyPolicyDeniesEverything(t *testing.T) {
	fake := runtimetest.NewFake()
	fake.EgressPolicy = true
	srv, key := newFakeRuntimeServer(t, fake)

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key,
		`{"template":"base","network":{"deny":["203.0.113.0/24"]}}`)
	if code != 201 {
		t.Fatalf("create with a deny rule returned %d (%v), want 201 on a runtime that enforces egress", code, out)
	}
	adv, ok := out["network_advisories"].([]any)
	if !ok || len(adv) == 0 {
		t.Fatalf("create answered %v with no network_advisories — the caller cannot tell this sandbox has no network at all", out)
	}
	joined := ""
	for _, a := range adv {
		s, _ := a.(string)
		joined += s + "\n"
	}
	if !strings.Contains(joined, "denies all egress") {
		t.Errorf("advisories %q do not say the policy denies all egress", joined)
	}

	// Negative control: an ordinary allow list of addresses is exactly what the
	// API is for, and must carry no advisory at all — otherwise the field is
	// noise every caller learns to ignore.
	code, out = do(t, "POST", srv.URL+"/v1/sandboxes", key,
		`{"template":"base","network":{"allow":["203.0.113.7","198.51.100.0/24"]}}`)
	if code != 201 {
		t.Fatalf("create with an address allow list returned %d (%v), want 201", code, out)
	}
	if adv, present := out["network_advisories"]; present {
		t.Errorf("an address allow list carried advisories %v; the field must stay quiet when the policy is unremarkable", adv)
	}
}
