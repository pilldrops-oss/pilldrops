package vm

import (
	"net"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/egress"
)

// The in-ns ruleset is the only egress control in netns mode.
func TestNetNSRulesetExpressesTheEgressPolicy(t *testing.T) {
	rs := buildNetNSRuleset("vn7", "tap0", net.ParseIP("10.128.0.29"), net.ParseIP("10.128.0.30"), net.ParseIP("192.168.30.2"), "", "", false)
	for _, want := range []string{
		"policy drop",
		"ip daddr 169.254.0.0/16 drop", // metadata
		"ip daddr 10.0.0.0/8 drop",     // RFC1918 plus every slot/host veth address
		"ip daddr 172.16.0.0/12 drop",  // RFC1918
		"ip daddr 100.64.0.0/10 drop",  // CGNAT
		"udp dport 53 accept",
		"tcp dport { 53, 80, 443 } accept",
		`iifname "vn7" ip daddr 10.128.0.30 dnat to 192.168.30.2`,
		"ct state established,related accept",
	} {
		if !strings.Contains(rs, want) {
			t.Errorf("in-ns ruleset lost %q:\n%s", want, rs)
		}
	}
	// Private-range drops must precede the port allow-list or 10.x:443 is reachable.
	if strings.Index(rs, "10.0.0.0/8 drop") > strings.Index(rs, "dport { 53, 80, 443 }") {
		t.Error("private-range drops rendered after the port allow-list — 10.x:443 would be reachable")
	}
}

// A rule set in force replaces the chain's open tail. If the default
// "tcp dport { 53, 80, 443 } accept" survived alongside it, every policy would
// still reach the whole web and the allow list would be decoration.
func TestEgressPolicyReplacesTheOpenDefaultTail(t *testing.T) {
	policy, divert, err := renderEgressPolicy(&egress.Rules{
		Allow: []string{"203.0.113.7"},
		Deny:  []string{egress.AllTraffic},
	}, "tap0")
	if err != nil {
		t.Fatalf("an address-only policy was refused: %v", err)
	}
	if divert != "" {
		t.Errorf("an address-only policy diverted to the name layer, which decides nothing it "+
			"could not: %q", divert)
	}
	rs := buildNetNSRuleset("vn7", "tap0", net.ParseIP("10.128.0.29"), net.ParseIP("10.128.0.30"), net.ParseIP("192.168.30.2"), policy, divert, false)

	if strings.Contains(rs, "tcp dport { 53, 80, 443 } accept") {
		t.Errorf("the open default survived under a policy, so the allow list decides nothing:\n%s", rs)
	}
	for _, want := range []string{
		"ip daddr 203.0.113.7/32 accept",
		"udp dport 53 ip daddr 8.8.8.8 accept", // else nothing in the sandbox resolves
		"ip daddr 0.0.0.0/0 drop",
	} {
		if !strings.Contains(rs, want) {
			t.Errorf("policy chain lost %q:\n%s", want, rs)
		}
	}
	// The platform drops are not negotiable: an allow entry must not buy a route
	// to another slot, the host veth, or the metadata service.
	if strings.Index(rs, "ip daddr 10.0.0.0/8 drop") > strings.Index(rs, "ip daddr 203.0.113.7/32 accept") {
		t.Errorf("a tenant allow entry rendered before the platform drops, so a policy naming "+
			"10.128.x.x would reach another sandbox:\n%s", rs)
	}
}

// A domain rule renders a divert into PREROUTING, never into the egress chain:
// `redirect` is a nat statement and the egress chain is a filter chain, so the
// same line there fails the load of the whole table and takes every rule with
// it — including the platform drops. This is the test that fails against the
// tree before the name layer landed.
func TestDomainRulesDivertFromPreroutingNotTheFilterChain(t *testing.T) {
	policy, divert, err := renderEgressPolicy(&egress.Rules{
		Allow: []string{"*.github.com", "203.0.113.7"},
		Deny:  []string{egress.AllTraffic},
	}, "tap0")
	if err != nil {
		t.Fatalf("a domain rule was refused on a node that runs the name layer: %v", err)
	}
	if strings.Contains(policy, "redirect") {
		t.Errorf("a `redirect` was rendered into the filter egress chain; nft refuses it there and "+
			"the whole table fails to load, so the sandbox has NO rules at all:\n%s", policy)
	}
	want := "tcp dport { 80, 443 } redirect to :" + strconv.Itoa(egressProxyPort)
	if !strings.Contains(divert, want) {
		t.Fatalf("no divert to the name layer was rendered, so the domain entry decides nothing:\n%s", divert)
	}
	// The reserved ranges return before the redirect. prerouting runs before
	// the forward hook the platform drops live on, so without this a guest
	// reaches the metadata service by putting a permitted name in a Host header.
	if i, j := strings.Index(divert, "169.254.0.0/16 return"), strings.Index(divert, want); i < 0 || i > j {
		t.Errorf("the link-local return does not precede the redirect, so 169.254.169.254:80 is "+
			"diverted into userspace past the in-ns drop:\n%s", divert)
	}
	if !strings.Contains(divert, `iifname "tap0"`) {
		t.Errorf("the divert is not keyed on the guest interface, so the node's own DNAT'd traffic "+
			"into the guest (previews, envd) is diverted into the proxy:\n%s", divert)
	}
	// An address the policy already allows is decided in the kernel.
	if !strings.Contains(divert, "ip daddr 203.0.113.7/32 return") {
		t.Errorf("an allow-listed address still takes a userspace hop for the same verdict:\n%s", divert)
	}

	if policy, divert, err := renderEgressPolicy(nil, "tap0"); err != nil || policy != "" || divert != "" {
		t.Errorf("no rule set rendered %q/%q (err %v), want the open default left alone", policy, divert, err)
	}
}

// PILLDROPS_LOCAL_API_PORT opens exactly one hole: the slot's host veth
// address on that port, ahead of the 10/8 drop. Unset, nothing is rendered.
func TestLocalAPIPortOpensOnePortToTheHostVeth(t *testing.T) {
	t.Setenv("PILLDROPS_LOCAL_API_PORT", "")
	host, ns, guest := net.ParseIP("10.128.0.29"), net.ParseIP("10.128.0.30"), net.ParseIP("192.168.30.2")
	base := buildNetNSRuleset("vn7", "tap0", host, ns, guest, "", "", false)
	if strings.Contains(base, "dport 18103") || localAPIHostRule() != "" {
		t.Fatal("a rule rendered with the variable unset")
	}
	t.Setenv("PILLDROPS_LOCAL_API_PORT", "18103")
	rs := buildNetNSRuleset("vn7", "tap0", host, ns, guest, "", "", false)
	rule := "ip daddr 10.128.0.29 tcp dport 18103 accept"
	i, j := strings.Index(rs, rule), strings.Index(rs, "ip daddr 10.0.0.0/8 drop")
	if i < 0 || j < 0 || i > j {
		t.Fatalf("the local-API accept must precede the 10/8 drop:\n%s", rs)
	}
	if strings.Replace(rs, "\t\t"+rule+"\n", "", 1) != base {
		t.Fatal("the variable changed more than the one accept line")
	}
	if got := localAPIHostRule(); !strings.Contains(got, `iifname "vh*" ip saddr 10.128.0.0/16 tcp dport 18103 accept`) {
		t.Fatalf("host rule = %q", got)
	}
	t.Setenv("PILLDROPS_LOCAL_API_PORT", "70000")
	if localAPIPort() != 0 {
		t.Fatal("an out-of-range port must read as unset")
	}
}

// Slot numbers name host links (vh<slot>) and 10.128/16 addresses, so two
// FC_NETNS daemons on one host must be able to start from different bases.
func TestNetNSSlotBase(t *testing.T) {
	for in, want := range map[string]int{"": 0, "200": 200, "-1": 0, "nonsense": 0} {
		t.Setenv("FC_NETNS_SLOT_BASE", in)
		if got := netnsSlotBase(); got != want {
			t.Errorf("FC_NETNS_SLOT_BASE=%q → %d, want %d", in, got, want)
		}
	}
}

// A node provisioned by scripts/provision-node.sh has docker, docker sets the
// base filter FORWARD chain's policy to DROP, and a drop there wins over the
// sandboxd_netns table however correct that table is. Measured on a real node
// 2026-09-22: no connectivity at any layer, in the DEFAULT network mode.
func TestNetNSBaseChainRulesLetSandboxTrafficPastADroppingFORWARD(t *testing.T) {
	rules := netnsBaseChainRules("DOCKER-USER")
	var flat []string
	for _, r := range rules {
		if r[0] != "DOCKER-USER" {
			t.Fatalf("rule %v does not name the chain it was asked for", r)
		}
		flat = append(flat, strings.Join(r, " "))
	}
	joined := strings.Join(flat, "\n")

	// Egress, and the replies to it: the fix proven on the node needed both,
	// because a DROP policy kills the FORWARD path in either direction.
	if !strings.Contains(joined, "-s "+netnsSupernet+" -j ACCEPT") {
		t.Errorf("nothing accepts traffic sourced from the slot supernet — sandboxes have no outbound network at all:\n%s", joined)
	}
	if !strings.Contains(joined, "-d "+netnsSupernet+" ") || !strings.Contains(joined, "RELATED,ESTABLISHED -j ACCEPT") {
		t.Errorf("nothing accepts replies back to the slot supernet — packets leave and nothing comes back:\n%s", joined)
	}
	// Not a hole: inbound is conntrack-scoped, so the world cannot open new
	// connections to a sandbox through the host's FORWARD path.
	for _, r := range rules {
		if r[1] == "-d" && !strings.Contains(strings.Join(r, " "), "--ctstate RELATED,ESTABLISHED") {
			t.Errorf("the inbound accept %v is unscoped — it admits new connections to sandboxes, not just replies", r)
		}
	}
	if got := netnsBaseChainRules("FORWARD")[0][0]; got != "FORWARD" {
		t.Errorf("chain argument ignored: got %q", got)
	}
}

// Composing the rules is not installing them. The one caller lives in
// //go:build linux source that never compiles on a macOS checkout, so it is
// read as text — the alternative is a test that passes because neither reading
// produces a value.
func TestEnsureNetNSHostRulesInstallsTheBaseChainAccepts(t *testing.T) {
	body, err := os.ReadFile("netns_linux.go")
	if err != nil {
		t.Fatalf("read netns_linux.go: %v", err)
	}
	src := string(body)
	start := strings.Index(src, "func EnsureNetNSHostRules")
	if start < 0 {
		t.Fatal("netns_linux.go no longer defines EnsureNetNSHostRules")
	}
	ensure := src[start:]
	if i := strings.Index(ensure, "\nfunc "); i > 0 {
		ensure = ensure[:i]
	}
	if !strings.Contains(ensure, "allowNetNSPastBaseChains()") {
		t.Fatal("EnsureNetNSHostRules does not call allowNetNSPastBaseChains().\n\n" +
			"The sandboxd_netns table is a different netfilter hook from the base\n" +
			"filter FORWARD chain, whose policy is DROP on every node that has\n" +
			"docker — which is every node provision-node.sh touches. Without this\n" +
			"call the rules above are composed and never reach the kernel, and the\n" +
			"default configuration serves sandboxes with no internet at all.")
	}
	if !strings.Contains(src, "netnsBaseChainRules(baseFilterChain())") {
		t.Error("allowNetNSPastBaseChains no longer derives its chain from baseFilterChain(); " +
			"a hardcoded DOCKER-USER breaks every node without docker whose FORWARD policy is DROP")
	}
}
