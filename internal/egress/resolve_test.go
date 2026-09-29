package egress

import (
	"strings"
	"testing"
)

func eq(t *testing.T, got, want []string, what string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: got %v, want %v", what, got, want)
		}
	}
}

func TestNarrowCannotWiden(t *testing.T) {
	base := &Rules{Allow: []string{"*.github.com"}, Deny: []string{AllTraffic}}
	req := &Rules{Allow: []string{"*.github.com", "*.evil.example"}}

	got := Narrow(base, req)
	eq(t, got.Allow, []string{"*.github.com"}, "allow after narrowing")

	allow, deny, err := got.Parse()
	if err != nil {
		t.Fatal(err)
	}
	if v := Match(allow, deny, conn("api.evil.example", "203.0.113.9", 443)); v.Allowed {
		t.Errorf("a request added a destination its org policy never permitted: %s", v.Reason)
	}
}

func TestNarrowCanRestrict(t *testing.T) {
	base := &Rules{Allow: []string{"*.github.com", "*.npmjs.org"}, Deny: []string{AllTraffic}}
	req := &Rules{Allow: []string{"*.github.com"}}

	got := Narrow(base, req)
	eq(t, got.Allow, []string{"*.github.com"}, "allow after narrowing")

	allow, deny, _ := got.Parse()
	if v := Match(allow, deny, conn("registry.npmjs.org", "203.0.113.9", 443)); v.Allowed {
		t.Errorf("a permission the request gave up was still in force: %s", v.Reason)
	}
}

// A request can add a deny but never remove one the base imposed.
func TestNarrowUnionsDeny(t *testing.T) {
	base := &Rules{Allow: []string{"*.github.com", "*.npmjs.org"}, Deny: []string{"1.1.1.1"}}
	req := &Rules{Allow: []string{"*.github.com", "*.npmjs.org"}, Deny: []string{"8.8.8.8"}}

	got := Narrow(base, req)
	eq(t, got.Deny, []string{"1.1.1.1", "8.8.8.8"}, "deny after narrowing")

	got2 := Narrow(base, &Rules{Allow: []string{"*.github.com"}})
	eq(t, got2.Deny, []string{"1.1.1.1"}, "base deny survives a request that omits it")
}

func TestNarrowWithNoBaseAcceptsTheRequest(t *testing.T) {
	req := &Rules{Allow: []string{"*.github.com"}, Deny: []string{AllTraffic}}
	got := Narrow(nil, req)
	if got != req {
		t.Fatalf("a request against no base policy was altered: %+v", got)
	}
}

func TestNarrowWithNoRequestKeepsTheBase(t *testing.T) {
	base := &Rules{Allow: []string{"*.github.com"}}
	if got := Narrow(base, nil); got != base {
		t.Fatalf("a base policy was altered by an absent request: %+v", got)
	}
}

// An empty allow list intersected with anything stays empty.
func TestNarrowCannotEscapeADenyAllBase(t *testing.T) {
	base := DenyAll()
	for _, req := range []*Rules{
		{Allow: []string{"*.github.com"}},
		{Allow: []string{AllTraffic}},
		{Deny: []string{}},
		{},
	} {
		got := Narrow(base, req)
		allow, deny, err := got.Parse()
		if err != nil {
			t.Fatalf("%+v: %v", req, err)
		}
		if v := Match(allow, deny, conn("anywhere.example", "203.0.113.9", 443)); v.Allowed {
			t.Errorf("request %+v escaped a deny-all base policy: %s", req, v.Reason)
		}
	}
}

// If DenyAll became &Rules{} or nil, every service template would regain unrestricted egress.
func TestDenyAllIsAPolicyNotAnAbsence(t *testing.T) {
	r := DenyAll()
	if r == nil {
		t.Fatal("DenyAll returned nil, which the matcher reads as 'no policy in force'")
	}
	allow, deny, err := r.Parse()
	if err != nil {
		t.Fatal(err)
	}
	if allow == nil && deny == nil {
		t.Fatal("DenyAll parsed to two nils — indistinguishable from an absent policy")
	}
	if v := Match(allow, deny, conn("anywhere.example", "203.0.113.9", 443)); v.Allowed {
		t.Errorf("DenyAll permitted traffic: %s", v.Reason)
	}
}

func TestNarrowComparesEntriesNormalised(t *testing.T) {
	base := &Rules{Allow: []string{"*.GitHub.com", "1.1.1.1"}}
	req := &Rules{Allow: []string{"*.github.com.", "1.1.1.1/32"}}

	got := Narrow(base, req)
	if len(got.Allow) != 2 {
		t.Errorf("differently-spelled but identical entries did not survive the intersection: %v", got.Allow)
	}
}

// Pins the coarseness noted on Narrow so a change to it is deliberate.
func TestNarrowDropsNarrowerRequestEntries(t *testing.T) {
	base := &Rules{Allow: []string{"*.github.com"}}
	req := &Rules{Allow: []string{"api.github.com"}}

	got := Narrow(base, req)
	if len(got.Allow) != 0 {
		t.Errorf("entry-equality intersection unexpectedly kept %v — if this now does subset "+
			"containment, that is a real improvement, but update Narrow's ponytail note and "+
			"this test together rather than leaving the doc claiming otherwise", got.Allow)
	}
}

// A database host allowed by name can never match; no name is on the wire for postgres.
func TestAdvisoryOnDomainOnlyAllowList(t *testing.T) {
	adv := (&Rules{Allow: []string{"db.internal.example"}, Deny: []string{AllTraffic}}).Advisories()
	if len(adv) == 0 {
		t.Fatal("a domain-only allow list produced no advisory; an operator writing this for " +
			"postgres on 5432 gets a policy that silently blocks it")
	}
	if !strings.Contains(adv[0], "443") || !strings.Contains(adv[0], "80") {
		t.Errorf("the advisory does not name the ports where domains DO work: %q", adv[0])
	}
}

func TestNoAdvisoryOnAMixedList(t *testing.T) {
	r := &Rules{Allow: []string{"*.github.com", "10.0.5.0/24"}, Deny: []string{AllTraffic}}
	for _, a := range r.Advisories() {
		if strings.Contains(a, "no address entries") {
			t.Errorf("a list that HAS address entries was advised about not having any: %q", a)
		}
	}
}

func TestAdvisoriesStayQuietWhereTheyShould(t *testing.T) {
	if adv := (*Rules)(nil).Advisories(); adv != nil {
		t.Errorf("an absent rule set produced advisories: %v", adv)
	}
	if adv := (&Rules{Allow: []string{"::1"}}).Advisories(); adv != nil {
		t.Errorf("an invalid rule set produced advisories instead of leaving it to Validate: %v", adv)
	}
}
