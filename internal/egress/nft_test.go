package egress

import (
	"strings"
	"testing"
)

func render(t *testing.T, allow, deny []string, opts ChainOpts) []string {
	t.Helper()
	a, d, err := (&Rules{Allow: allow, Deny: deny}).Parse()
	if err != nil {
		t.Fatalf("fixture rules did not parse: %v", err)
	}
	return RenderEgressChain(a, d, opts)
}

func indexOf(lines []string, substr string) int {
	for i, l := range lines {
		if strings.Contains(l, substr) {
			return i
		}
	}
	return -1
}

func TestRenderNothingForAnAbsentRuleSet(t *testing.T) {
	if lines := RenderEgressChain(nil, nil, ChainOpts{ProxyPort: 9999}); lines != nil {
		t.Errorf("an absent rule set rendered policy rules: %v", lines)
	}
}

func TestRenderedChainEndsInDrop(t *testing.T) {
	lines := render(t, []string{"1.1.1.1"}, nil, ChainOpts{})
	if len(lines) == 0 || lines[len(lines)-1] != "drop" {
		t.Errorf("a rule set in force did not end in a default drop: %v", lines)
	}
}

func divert(t *testing.T, allow, deny []string, opts ChainOpts) []string {
	t.Helper()
	a, d, err := (&Rules{Allow: allow, Deny: deny}).Parse()
	if err != nil {
		t.Fatalf("fixture rules did not parse: %v", err)
	}
	return RenderDivertRule(a, d, opts)
}

// `redirect` is a nat statement. In the filter egress chain nft refuses it and
// the whole table fails to load, which leaves the sandbox with NO rules — not
// even the platform drops. It belongs in prerouting, which is an earlier hook
// and therefore still ahead of every address denial in the chain.
func TestTheDivertIsNeverRenderedIntoTheFilterChain(t *testing.T) {
	lines := render(t, []string{"*.github.com"}, []string{AllTraffic}, ChainOpts{ProxyPort: 9999, IifName: "tap0"})
	if i := indexOf(lines, "redirect"); i >= 0 {
		t.Errorf("a nat `redirect` was rendered into the filter chain (line %d), which fails the "+
			"load of the entire table:\n%s", i, strings.Join(lines, "\n"))
	}
	d := divert(t, []string{"*.github.com"}, []string{AllTraffic}, ChainOpts{ProxyPort: 9999, IifName: "tap0"})
	if indexOf(d, "redirect to :9999") < 0 {
		t.Fatalf("no divert was emitted for a rule set with domain entries: %v", d)
	}
}

// prerouting runs before the forward hook the platform drops live on, so a
// blanket redirect there is a way past them: a guest that sends an allowed Host
// header to 169.254.169.254 would be proxied to the metadata service.
func TestReservedRangesReturnBeforeTheRedirect(t *testing.T) {
	d := divert(t, []string{"*.github.com"}, []string{AllTraffic}, ChainOpts{ProxyPort: 9999, IifName: "tap0"})
	red := indexOf(d, "redirect to :9999")
	for _, p := range ReservedPrefixes() {
		i := indexOf(d, "ip daddr "+p.String()+" return")
		if i < 0 || i > red {
			t.Errorf("%s is not returned before the redirect, so it is diverted into userspace "+
				"past the in-namespace drop that protects it:\n%s", p, strings.Join(d, "\n"))
		}
	}
}

func TestAddressAllowReturnsBeforeTheDivert(t *testing.T) {
	d := divert(t, []string{"1.1.1.1", "*.github.com"}, []string{AllTraffic}, ChainOpts{ProxyPort: 9999, IifName: "tap0"})
	allow := indexOf(d, "ip daddr 1.1.1.1/32 return")
	red := indexOf(d, "redirect to :9999")
	if allow < 0 || red < 0 {
		t.Fatalf("missing rules: %v", d)
	}
	if allow > red {
		t.Errorf("an explicit address allow (line %d) sits after the divert (line %d), so an "+
			"allow-listed IP takes a userspace hop for a verdict the kernel already had:\n%s",
			allow, red, strings.Join(d, "\n"))
	}
}

func TestNoDivertWithoutDomainEntries(t *testing.T) {
	opts := ChainOpts{ProxyPort: 9999, IifName: "tap0"}
	if d := divert(t, []string{"1.1.1.1", "8.8.8.0/24"}, []string{AllTraffic}, opts); d != nil {
		t.Errorf("an address-only rule set diverted to userspace anyway: %v", d)
	}
	// No name layer, or no interface to key the rule on: nothing is diverted,
	// and RenderEgressChainErr is what refuses the rule set.
	if d := divert(t, []string{"*.github.com"}, nil, ChainOpts{IifName: "tap0"}); d != nil {
		t.Errorf("a divert was rendered with no name layer to receive it: %v", d)
	}
	if d := divert(t, []string{"*.github.com"}, nil, ChainOpts{ProxyPort: 9999}); d != nil {
		t.Errorf("a divert was rendered with no guest interface, so it would also capture the "+
			"node's own DNAT'd traffic into the guest: %v", d)
	}
}

func TestResolverIsOpenedOnlyToTheGuestsNameservers(t *testing.T) {
	dns := ChainOpts{ProxyPort: 9999, ResolverAddrs: []string{"8.8.8.8", "1.1.1.1"}}

	with := render(t, []string{"203.0.113.7"}, []string{AllTraffic}, dns)
	if indexOf(with, "udp dport 53 ip daddr 8.8.8.8 accept") != 0 {
		t.Errorf("the resolver allowance is missing or not first — every name lookup in the "+
			"sandbox fails and the allow list reads as broken: %v", with)
	}
	if indexOf(with, "udp dport 53 accept") >= 0 {
		t.Errorf("UDP 53 was opened to every destination, which is a tunnel out of a sandbox "+
			"whose policy permits one address: %v", with)
	}

	// A policy that permits nothing is "no network", and DNS is network.
	denyAll := render(t, nil, []string{AllTraffic}, dns)
	if i := indexOf(denyAll, "udp dport 53"); i >= 0 {
		t.Errorf("a deny-everything policy still resolves names (line %d), which is a channel "+
			"out of a sandbox asked to have none: %v", i, denyAll)
	}

	without := render(t, []string{"203.0.113.7"}, []string{AllTraffic}, ChainOpts{ProxyPort: 9999})
	if indexOf(without, "udp dport 53") >= 0 {
		t.Errorf("a resolver allowance was emitted without being asked for: %v", without)
	}
}

func TestDomainEntriesWithoutANameLayerAreRefused(t *testing.T) {
	a, d, err := (&Rules{Allow: []string{"*.github.com"}, Deny: []string{AllTraffic}}).Parse()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := RenderEgressChainErr(a, d, ChainOpts{ProxyPort: 0}); err == nil {
		t.Error("a rule set with domain entries rendered cleanly with no proxy to evaluate them; " +
			"the resulting chain silently drops the traffic the policy says it allows")
	}
	// The address-only case is fine without a name layer.
	a2, d2, _ := (&Rules{Allow: []string{"1.1.1.1"}, Deny: []string{AllTraffic}}).Parse()
	if _, err := RenderEgressChainErr(a2, d2, ChainOpts{ProxyPort: 0}); err != nil {
		t.Errorf("an address-only rule set was refused for lack of a name layer it does not need: %v", err)
	}
}

// A domain in the rendered chain is an nft syntax error that takes the whole table down.
func TestDomainsNeverReachTheKernelChain(t *testing.T) {
	lines := render(t, []string{"*.github.com", "api.example.org"}, []string{AllTraffic}, ChainOpts{ProxyPort: 9999})
	for _, l := range lines {
		if strings.HasPrefix(l, "#") {
			continue
		}
		if strings.Contains(l, "github.com") || strings.Contains(l, "example.org") {
			t.Errorf("a domain reached the kernel ruleset, which nft cannot parse — the load "+
				"fails and every sandbox on this node loses egress: %q", l)
		}
	}
}

// An ip6 match in a `table ip` chain is a load error for the entire table.
func TestRenderedChainIsIPv4Only(t *testing.T) {
	lines := render(t, []string{"1.1.1.1", "8.8.8.0/24"}, []string{AllTraffic}, ChainOpts{})
	for _, l := range lines {
		if strings.Contains(l, "ip6 ") {
			t.Errorf("an ip6 match was rendered into a `table ip` chain; nft refuses to load the "+
				"whole table: %q", l)
		}
	}
}

func TestChainText(t *testing.T) {
	if got := ChainText(nil); got != "" {
		t.Errorf("an empty render produced %q, which would land as a blank line in the ruleset", got)
	}
	got := ChainText([]string{"a", "b"})
	if !strings.HasPrefix(got, "\t\t") || !strings.Contains(got, "\n\t\tb") {
		t.Errorf("indentation does not match the surrounding ruleset body: %q", got)
	}
}
