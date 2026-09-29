package egress

import (
	"net/netip"
	"strings"
	"testing"
)

// mustRules parses a rule set the way an operator would write it.
func mustRules(t *testing.T, allow, deny []string) (a, d []Entry) {
	t.Helper()
	r := &Rules{Allow: allow, Deny: deny}
	a, d, err := r.Parse()
	if err != nil {
		t.Fatalf("fixture rules did not parse: %v", err)
	}
	return a, d
}

func conn(name, addr string, port int) Conn {
	c := Conn{Name: name, Port: port}
	if addr != "" {
		c.Addr = netip.MustParseAddr(addr)
	}
	return c
}

func TestNilRulesAreUnrestricted(t *testing.T) {
	v := Match(nil, nil, conn("anywhere.example", "93.184.216.34", 443))
	if !v.Allowed {
		t.Fatalf("a sandbox with NO egress rules was denied — this is the pre-feature default "+
			"and turning it into a deny breaks every existing workload: %s", v.Reason)
	}
}

// This is the half that fails open if anyone writes `if len(allow) == 0 { allow all }`.
func TestEmptyRulesDenyEverything(t *testing.T) {
	allow, deny := mustRules(t, []string{}, []string{})
	if allow == nil && deny == nil {
		t.Fatal("fixture: a PRESENT but empty rule set parsed to two nil slices, which is " +
			"indistinguishable from an absent one — the distinction this test exists for")
	}
	v := Match(allow, deny, conn("anywhere.example", "93.184.216.34", 443))
	if v.Allowed {
		t.Fatalf("an empty rule set permitted traffic. Empty means DENY; only an ABSENT "+
			"rule set means unrestricted. This is how a locked-down service template "+
			"silently regains full egress: %s", v.Reason)
	}
}

// "Deny everything, then name exceptions" only works because allow beats deny.
func TestAllowBeatsDeny(t *testing.T) {
	allow, deny := mustRules(t, []string{"*.github.com"}, []string{AllTraffic})

	if v := Match(allow, deny, conn("api.github.com", "140.82.121.5", 443)); !v.Allowed {
		t.Errorf("deny 0.0.0.0/0 + allow *.github.com denied api.github.com. Under deny-wins "+
			"this policy denies everything and reads to its author as broken, which is why "+
			"allow must win: %s", v.Reason)
	}
	if v := Match(allow, deny, conn("evil.example", "203.0.113.9", 443)); v.Allowed {
		t.Errorf("the blanket deny stopped applying to unlisted hosts: %s", v.Reason)
	}
}

func TestUnmatchedIsDeniedWhenRulesExist(t *testing.T) {
	allow, deny := mustRules(t, []string{"*.github.com"}, nil)
	if v := Match(allow, deny, conn("elsewhere.example", "203.0.113.9", 443)); v.Allowed {
		t.Errorf("a destination in no list was allowed: %s", v.Reason)
	}
}

func TestWildcardMatching(t *testing.T) {
	allow, deny := mustRules(t, []string{"*.github.com"}, []string{AllTraffic})

	for _, tc := range []struct {
		name  string
		host  string
		allow bool
		why   string
	}{
		{"subdomain", "api.github.com", true, "the case the entry is written for"},
		{"deep subdomain", "a.b.github.com", true, `providers document "*." as all subdomains, not one label`},
		{"case insensitive", "API.GitHub.COM", true, "DNS is case-insensitive; SNI can arrive in any case"},
		{"trailing dot", "api.github.com.", true, "a fully qualified name with the root dot is the same name"},
		{"bare parent", "github.com", false, `"*.github.com" covers subdomains, not the parent — an operator who wants both writes both`},
		{"suffix confusion", "evilgithub.com", false, "THE classic allow-list hole: matching on the bare domain instead of anchoring on the dot"},
		{"prefix confusion", "github.com.evil.example", false, "the entry must anchor at the END of the name, not appear anywhere in it"},
		{"unrelated", "gitlab.com", false, "a different host entirely"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := Match(allow, deny, conn(tc.host, "203.0.113.9", 443))
			if v.Allowed != tc.allow {
				t.Errorf("*.github.com vs %q: allowed=%v, want %v (%s) — %s",
					tc.host, v.Allowed, tc.allow, tc.why, v.Reason)
			}
		})
	}
}

func TestExactDomainDoesNotCoverSubdomains(t *testing.T) {
	allow, deny := mustRules(t, []string{"github.com"}, []string{AllTraffic})
	if v := Match(allow, deny, conn("github.com", "140.82.121.5", 443)); !v.Allowed {
		t.Errorf("an exact entry did not match its own host: %s", v.Reason)
	}
	if v := Match(allow, deny, conn("api.github.com", "140.82.121.5", 443)); v.Allowed {
		t.Errorf("an exact entry matched a subdomain — an operator listing one host got a "+
			"whole tree: %s", v.Reason)
	}
}

// Otherwise a sandbox reaches a denied host by not announcing where it is going.
func TestDomainEntryCannotBeBorrowedWithoutAName(t *testing.T) {
	allow, deny := mustRules(t, []string{"*.github.com"}, []string{AllTraffic})

	for _, tc := range []struct {
		name string
		c    Conn
		why  string
	}{
		{"no SNI on 443", conn("", "140.82.121.5", 443), "a ClientHello with no SNI carries no name to match"},
		{"raw TCP on 5432", conn("", "140.82.121.5", 5432), "no name is on the wire for postgres"},
		{"name claimed on a non-name port", conn("api.github.com", "140.82.121.5", 5432),
			"nothing on 5432 can be trusted to carry a name; only the address may decide"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if v := Match(allow, deny, tc.c); v.Allowed {
				t.Errorf("a domain entry decided a connection with no matchable name (%s): %s", tc.why, v.Reason)
			}
		})
	}
}

func TestAddressEntryDecidesNamelessTraffic(t *testing.T) {
	allow, deny := mustRules(t, []string{"140.82.121.0/24"}, []string{AllTraffic})
	for _, c := range []Conn{
		conn("", "140.82.121.5", 443),
		conn("", "140.82.121.5", 5432),
	} {
		if v := Match(allow, deny, c); !v.Allowed {
			t.Errorf("port %d to an allow-listed CIDR was denied: %s", c.Port, v.Reason)
		}
	}
	if v := Match(allow, deny, conn("", "203.0.113.9", 5432)); v.Allowed {
		t.Errorf("an address outside the CIDR was allowed: %s", v.Reason)
	}
}

func TestAddrMatching(t *testing.T) {
	allow, deny := mustRules(t, []string{"1.1.1.1", "8.8.8.0/24"}, []string{AllTraffic})
	for _, tc := range []struct {
		addr  string
		allow bool
	}{
		{"1.1.1.1", true},   // bare address, normalised to /32
		{"1.1.1.2", false},  // adjacent, must not be covered
		{"8.8.8.8", true},   // inside the /24
		{"8.8.8.255", true}, // still inside
		{"8.8.9.1", false},  // just outside
	} {
		v := Match(allow, deny, conn("", tc.addr, 443))
		if v.Allowed != tc.allow {
			t.Errorf("%s: allowed=%v want %v — %s", tc.addr, v.Allowed, tc.allow, v.Reason)
		}
	}
}

func TestAllTrafficCoversEverything(t *testing.T) {
	_, deny := mustRules(t, nil, []string{AllTraffic})
	for _, a := range []string{"1.1.1.1", "203.0.113.9", "140.82.121.5", "8.8.8.8"} {
		if v := Match(nil, deny, conn("", a, 443)); v.Allowed {
			t.Errorf("deny %s did not cover %s: %s", AllTraffic, a, v.Reason)
		}
	}
}

func TestVerdictNamesTheDecidingRule(t *testing.T) {
	allow, deny := mustRules(t, []string{"*.github.com"}, []string{AllTraffic})

	if v := Match(allow, deny, conn("api.github.com", "140.82.121.5", 443)); !strings.Contains(v.Reason, "*.github.com") {
		t.Errorf("an allow verdict does not name the rule that allowed it: %q", v.Reason)
	}
	if v := Match(allow, deny, conn("evil.example", "203.0.113.9", 443)); !strings.Contains(v.Reason, AllTraffic) {
		t.Errorf("a deny verdict does not name the rule that denied it: %q", v.Reason)
	}
	if v := Match(allow, nil, conn("evil.example", "203.0.113.9", 443)); v.Reason == "" {
		t.Error("a destination matched by no rule produced an empty reason")
	}
}
