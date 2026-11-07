package egress

import (
	"fmt"
	"strings"
	"testing"
)

// A nil *Rules means unrestricted; &Rules{} means deny everything.
func TestNilRulesAndEmptyRulesParseDifferently(t *testing.T) {
	var absent *Rules
	a1, d1, err := absent.Parse()
	if err != nil {
		t.Fatalf("a nil rule set failed to parse: %v", err)
	}
	if a1 != nil || d1 != nil {
		t.Errorf("an ABSENT rule set parsed to non-nil slices (%v, %v) — the matcher reads "+
			"nil as 'no policy', so this would turn every unconfigured sandbox into a "+
			"deny-everything one", a1, d1)
	}

	present := &Rules{}
	a2, d2, err := present.Parse()
	if err != nil {
		t.Fatalf("an empty rule set failed to parse: %v", err)
	}
	if a2 == nil && d2 == nil {
		t.Error("a PRESENT but empty rule set parsed to two nils, which the matcher reads as " +
			"'no policy in force'. That is the fail-open collapse: an operator who sets an " +
			"empty policy to deny everything would get unrestricted egress instead.")
	}
	if v := Match(a2, d2, conn("anywhere.example", "203.0.113.9", 443)); v.Allowed {
		t.Errorf("an explicitly empty policy permitted traffic: %s", v.Reason)
	}
}

func TestParseEntryClassifies(t *testing.T) {
	for _, tc := range []struct {
		raw      string
		kind     Kind
		wildcard bool
		domain   string
	}{
		{"1.1.1.1", KindAddr, false, ""},
		{"8.8.8.0/24", KindAddr, false, ""},
		{AllTraffic, KindAddr, false, ""},
		{"github.com", KindDomain, false, "github.com"},
		{"*.github.com", KindDomain, true, "github.com"},
		{"API.GitHub.com", KindDomain, false, "api.github.com"}, // normalised
		{"github.com.", KindDomain, false, "github.com"},        // root dot stripped
	} {
		e, err := ParseEntry(tc.raw)
		if err != nil {
			t.Errorf("%q: %v", tc.raw, err)
			continue
		}
		if e.Kind != tc.kind || e.Wildcard != tc.wildcard || e.Domain != tc.domain {
			t.Errorf("%q: kind=%v wildcard=%v domain=%q, want %v/%v/%q",
				tc.raw, e.Kind, e.Wildcard, e.Domain, tc.kind, tc.wildcard, tc.domain)
		}
	}
}

func TestBareAddressNormalisesToSlash32(t *testing.T) {
	e, err := ParseEntry("1.1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	if got := e.Prefix.String(); got != "1.1.1.1/32" {
		t.Errorf("bare address became %q, want 1.1.1.1/32", got)
	}
}

// Nothing on the node evaluates IPv6, so an accepted entry would be silently inert.
func TestIPv6EntriesAreRefused(t *testing.T) {
	for _, raw := range []string{"2606:4700:4700::1111", "2606:4700::/32", "::1", "::/0"} {
		_, err := ParseEntry(raw)
		if err == nil {
			t.Errorf("%q was accepted. Nothing on the node evaluates IPv6, so this rule would "+
				"be silently inert while reading as protection", raw)
			continue
		}
		if !strings.Contains(err.Error(), "IPv6") {
			t.Errorf("%q was refused but the error does not say why: %v", raw, err)
		}
	}
}

func TestMalformedEntriesAreRefused(t *testing.T) {
	for _, tc := range []struct{ raw, why string }{
		{"", "empty entry"},
		{"   ", "whitespace only"},
		{"localhost", "not fully qualified — no dot"},
		{"https://github.com", "carries a scheme"},
		{"github.com/path", "carries a path"},
		{"github.com:443", "carries a port"},
		{"*.*.github.com", "a second wildcard reads as a pattern and would match literally"},
		{"api.*.github.com", "an interior wildcard reads as a pattern and would match literally"},
		{"*.", "no domain after the wildcard"},
		{"10.0.0.0/33", "impossible prefix length"},
		{"999.1.1.1", "not an address and not a domain"},
	} {
		if _, err := ParseEntry(tc.raw); err == nil {
			t.Errorf("%q was accepted (%s)", tc.raw, tc.why)
		}
	}
}

func TestListCapsAreEnforcedAndExplained(t *testing.T) {
	domains := make([]string, MaxDomainEntries+1)
	for i := range domains {
		domains[i] = fmt.Sprintf("host%d.example.com", i)
	}
	err := (&Rules{Allow: domains}).Validate()
	if err == nil {
		t.Fatalf("%d domain entries were accepted, cap is %d", len(domains), MaxDomainEntries)
	}
	if !strings.Contains(err.Error(), fmt.Sprint(MaxDomainEntries)) {
		t.Errorf("the cap error does not name the limit, so a caller cannot tell how far over "+
			"they are: %v", err)
	}

	addrs := make([]string, MaxAddrEntries+1)
	for i := range addrs {
		addrs[i] = fmt.Sprintf("10.%d.0.0/16", i)
	}
	if err := (&Rules{Deny: addrs}).Validate(); err == nil {
		t.Fatalf("%d address entries were accepted, cap is %d", len(addrs), MaxAddrEntries)
	}
}

func TestAtTheCapIsAccepted(t *testing.T) {
	domains := make([]string, MaxDomainEntries)
	for i := range domains {
		domains[i] = fmt.Sprintf("host%d.example.com", i)
	}
	addrs := make([]string, MaxAddrEntries)
	for i := range addrs {
		addrs[i] = fmt.Sprintf("10.%d.0.0/16", i)
	}
	if err := (&Rules{Allow: domains, Deny: addrs}).Validate(); err != nil {
		t.Errorf("a rule set exactly at both caps was refused: %v", err)
	}
}

func TestCapsCountPerKindNotPerList(t *testing.T) {
	var mixed []string
	for i := range MaxDomainEntries {
		mixed = append(mixed, fmt.Sprintf("host%d.example.com", i))
	}
	for i := range MaxAddrEntries {
		// Public ranges: an allow entry covering a private one is refused on its own merits.
		mixed = append(mixed, fmt.Sprintf("198.51.100.%d/32", i))
	}
	if err := (&Rules{Allow: mixed}).Validate(); err != nil {
		t.Errorf("a list with %d domains and %d addresses — within both caps — was refused: %v",
			MaxDomainEntries, MaxAddrEntries, err)
	}
}

func TestBothListsAreReported(t *testing.T) {
	err := (&Rules{Allow: []string{"::1"}, Deny: []string{"also bad"}}).Validate()
	if err == nil {
		t.Fatal("two invalid lists parsed cleanly")
	}
	if !strings.Contains(err.Error(), "allow") || !strings.Contains(err.Error(), "deny") {
		t.Errorf("only one list was reported; a caller fixing this policy needs a round trip "+
			"per broken entry: %v", err)
	}
}

func TestNameIsOnTheWire(t *testing.T) {
	for _, p := range []int{PortHTTP, PortHTTPS} {
		if !NameIsOnTheWire(p) {
			t.Errorf("port %d carries a name (Host / SNI) and was reported as not", p)
		}
	}
	for _, p := range []int{22, 53, 5432, 6379, 8080, 8443} {
		if NameIsOnTheWire(p) {
			t.Errorf("port %d was reported as carrying a name. 8080/8443 are the tempting "+
				"ones and they are still wrong: the platform reads names off TLS and HTTP "+
				"at their registered ports, and a domain rule that silently applied "+
				"elsewhere would be filtering nothing", p)
		}
	}
}

// Each of these fails IP parsing and then looks like a dotted hostname.
func TestMistypedAddressesAreNotAcceptedAsDomains(t *testing.T) {
	for _, raw := range []string{"999.1.1.1", "10.0.0.256", "1.2.3.4.5", "192.168.1"} {
		if _, err := ParseEntry(raw); err == nil {
			t.Errorf("%q was accepted as a domain rule; it can never match anything", raw)
		}
	}
	// Hostnames that merely start with digits are legal.
	for _, raw := range []string{"1password.com", "3m.co.uk", "*.4chan.org", "s3.amazonaws.com"} {
		if _, err := ParseEntry(raw); err != nil {
			t.Errorf("%q is a legitimate hostname and was refused: %v", raw, err)
		}
	}
}

// An allow entry that covers a reserved range is a privilege escalation, not a
// restriction: "allow 0.0.0.0/0" accepts every port to every host, where the
// default chain permits 53/80/443, and "allow 10.0.0.0/8" is a route to the
// node and to every other sandbox. A deny may still name them.
func TestAllowEntriesMustBePublicDestinations(t *testing.T) {
	for _, bad := range []string{
		AllTraffic,        // every port to the whole internet
		"10.0.0.0/8",      // the node and the other sandboxes
		"10.128.0.5",      // one other sandbox's veth
		"169.254.169.254", // cloud metadata
		"192.168.30.1",    // the namespace gateway
		"127.0.0.1",       // the guest's own loopback
		"172.16.0.0/12",   // RFC1918
		"203.0.113.0/1",   // a prefix wide enough to swallow the private ranges
	} {
		if err := (&Rules{Allow: []string{bad}}).Validate(); err == nil {
			t.Errorf("allow %q was accepted; a policy naming it grants more than a sandbox with no policy at all", bad)
		}
	}

	// The same values are legal in a deny list, and public entries still pass.
	for _, ok := range []*Rules{
		{Deny: []string{AllTraffic}},
		{Deny: []string{"10.0.0.0/8", "169.254.169.254"}},
		{Allow: []string{"203.0.113.7", "198.51.100.0/24"}, Deny: []string{AllTraffic}},
	} {
		if err := ok.Validate(); err != nil {
			t.Errorf("%+v was refused: %v", ok, err)
		}
	}
}

// An allow wildcard over a public suffix admits every registrant under it.
func TestCheckWildcardsRefusesPublicSuffixAllows(t *testing.T) {
	for _, tc := range []struct {
		rules  Rules
		refuse bool
	}{
		{Rules{Allow: []string{"*.co.uk"}}, true},          // ICANN section
		{Rules{Allow: []string{"*.github.io"}}, true},      // private section: shared hosting
		{Rules{Allow: []string{"*.herokuapp.com"}}, true},  // private section
		{Rules{Allow: []string{"*.example.co.uk"}}, false}, // one registrant
		{Rules{Allow: []string{"*.pages.example.com"}}, false},
		{Rules{Allow: []string{"co.uk", "api.github.com"}}, false}, // exact names admit one host
		{Rules{Deny: []string{"*.co.uk"}}, false},                  // a deny only narrows
	} {
		err := CheckWildcards(&tc.rules)
		if tc.refuse && err == nil {
			t.Errorf("%+v was accepted; a wildcard over a public suffix allows every site registered under it", tc.rules)
		}
		if !tc.refuse && err != nil {
			t.Errorf("%+v was refused: %v", tc.rules, err)
		}
	}
	if err := CheckWildcards(nil); err != nil {
		t.Errorf("no rules refused: %v", err)
	}
}
