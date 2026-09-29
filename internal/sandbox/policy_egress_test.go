package sandbox

import (
	"errors"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/egress"
)

func resolveNet(t *testing.T, s Spec, tmpl TemplateDefaults, org *egress.Rules) Resolved {
	t.Helper()
	res, err := Resolve(s, tmpl, Limits{MaxVCPU: 8, MaxMemMB: 16384, MaxStorageMB: 102400, SessionTTLHrs: 24}, Fleet{}, org)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return res
}

// A regression here would silently restrict every sandbox on the fleet.
func TestResolveLeavesOrdinaryTemplatesUnrestricted(t *testing.T) {
	res := resolveNet(t, Spec{Template: "base"}, TemplateDefaults{ID: "base"}, nil)
	if res.Network != nil {
		t.Errorf("a template that declares no egress policy resolved to one: %+v", res.Network)
	}
}

func TestResolveDeniesServiceTemplateEgress(t *testing.T) {
	tmpl := TemplateDefaults{ID: "postgres", Network: egress.DenyAll()}

	res := resolveNet(t, Spec{Template: "postgres"}, tmpl, nil)
	if res.Network == nil {
		t.Fatal("a service template resolved to NO policy — it would have unrestricted egress")
	}
	allow, deny, err := res.Network.Parse()
	if err != nil {
		t.Fatal(err)
	}
	if v := egress.Match(allow, deny, egress.Conn{Name: "evil.example", Port: 443}); v.Allowed {
		t.Errorf("a service template reached the internet: %s", v.Reason)
	}

	// A request asking for the world does not get it.
	res = resolveNet(t, Spec{Template: "postgres", Network: &egress.Rules{Allow: []string{egress.AllTraffic}}}, tmpl, nil)
	allow, deny, _ = res.Network.Parse()
	if v := egress.Match(allow, deny, egress.Conn{Name: "evil.example", Port: 443}); v.Allowed {
		t.Errorf("a create request widened a service template's deny-all policy: %s", v.Reason)
	}
}

func TestOrgOverrideReplacesTemplateDefault(t *testing.T) {
	tmpl := TemplateDefaults{ID: "postgres", Network: egress.DenyAll()}
	// A public range: an allow entry covering RFC1918 is refused, since the
	// platform drops those destinations before any tenant rule is read.
	org := &egress.Rules{Allow: []string{"replica.internal.example", "198.51.100.0/24"}, Deny: []string{egress.AllTraffic}}

	res := resolveNet(t, Spec{Template: "postgres"}, tmpl, org)
	allow, deny, err := res.Network.Parse()
	if err != nil {
		t.Fatal(err)
	}
	if v := egress.Match(allow, deny, egress.Conn{Name: "replica.internal.example", Port: 443}); !v.Allowed {
		t.Errorf("the org override did not take effect over the template default: %s", v.Reason)
	}
	// Still a policy, not an absence.
	if v := egress.Match(allow, deny, egress.Conn{Name: "evil.example", Port: 443}); v.Allowed {
		t.Errorf("the org override left egress open: %s", v.Reason)
	}
}

func TestRequestNarrowsButCannotWidenOrgPolicy(t *testing.T) {
	org := &egress.Rules{Allow: []string{"*.github.com", "*.npmjs.org"}, Deny: []string{egress.AllTraffic}}

	res := resolveNet(t, Spec{
		Template: "base",
		Network:  &egress.Rules{Allow: []string{"*.github.com", "*.evil.example"}},
	}, TemplateDefaults{ID: "base"}, org)

	allow, deny, err := res.Network.Parse()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		host  string
		allow bool
		why   string
	}{
		{"api.github.com", true, "kept: in both the org policy and the request"},
		{"registry.npmjs.org", false, "given up: the request did not ask for it"},
		{"api.evil.example", false, "WIDENING: the request asked for a host the org never permitted"},
	} {
		if v := egress.Match(allow, deny, egress.Conn{Name: tc.host, Port: 443}); v.Allowed != tc.allow {
			t.Errorf("%s: allowed=%v want %v (%s) — %s", tc.host, v.Allowed, tc.allow, tc.why, v.Reason)
		}
	}
}

func TestInvalidNetworkRulesAreACallerError(t *testing.T) {
	_, err := Resolve(
		Spec{Template: "base", Network: &egress.Rules{Allow: []string{"::1"}}},
		TemplateDefaults{ID: "base"},
		Limits{MaxVCPU: 8, MaxMemMB: 16384, MaxStorageMB: 102400, SessionTTLHrs: 24},
		Fleet{}, nil)
	if err == nil {
		t.Fatal("an IPv6 rule entry was accepted")
	}
	var inv *InvalidError
	if !errorsAs(err, &inv) {
		t.Fatalf("a malformed rule produced %T, which the HTTP layer will not map to 400: %v", err, err)
	}
	if !strings.Contains(err.Error(), "network") {
		t.Errorf("the error does not say which part of the request was wrong: %v", err)
	}
}

// Advisories flag a legal but probably unintended policy without refusing it.
func TestResolveReportsAdvisories(t *testing.T) {
	res := resolveNet(t, Spec{
		Template: "base",
		Network:  &egress.Rules{Allow: []string{"db.internal.example"}, Deny: []string{egress.AllTraffic}},
	}, TemplateDefaults{ID: "base"}, nil)

	if len(res.NetworkAdvisories) == 0 {
		t.Error("a domain-only allow list produced no advisory; an operator writing this for a " +
			"database on 5432 gets a policy that silently blocks it")
	}
}

func errorsAs(err error, target any) bool { return errors.As(err, target) }
