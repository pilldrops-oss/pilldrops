//go:build linux

package vm

import (
	"net"
	"strings"
	"testing"
)

// Every slot must stay inside 10.128.0.0/16, the supernet the inter-slot drop is keyed on.
func TestSlotAddressing(t *testing.T) {
	seen := map[string]int{}
	_, supernet, err := net.ParseCIDR(netnsSupernet)
	if err != nil {
		t.Fatalf("netnsSupernet %q does not parse: every host rule and base-chain accept names it", netnsSupernet)
	}
	for _, slot := range []int{0, 1, 63, 64, 255, 256, 4095, netnsMaxSlots - 1} {
		host, ns := slotIP(slot, 1), slotIP(slot, 2)
		for _, ip := range []net.IP{host, ns} {
			if !supernet.Contains(ip) {
				t.Fatalf("slot %d address %s escapes %s — the inter-slot/VM-host drops are keyed on that supernet", slot, ip, supernet)
			}
			if prev, dup := seen[ip.String()]; dup {
				t.Fatalf("slot %d reuses address %s already held by slot %d", slot, ip, prev)
			}
			seen[ip.String()] = slot
		}
	}
	if slotIP(5, 1).String() != "10.128.0.21" || slotIP(5, 2).String() != "10.128.0.22" {
		t.Fatalf("slot 5 packed as %s/%s, want 10.128.0.21/10.128.0.22", slotIP(5, 1), slotIP(5, 2))
	}
	// Octet rollover.
	if slotIP(64, 1).String() != "10.128.1.1" {
		t.Fatalf("slot 64 host end is %s, want 10.128.1.1 (256/4 slots per octet)", slotIP(64, 1))
	}
}

func TestNetNSNameRoundTrip(t *testing.T) {
	for _, slot := range []int{0, 7, netnsMaxSlots - 1} {
		name := netnsNameFor(slot, "sb-1788415903084-3817ea76")
		s, id, ok := parseNetNSName(name)
		if !ok || s != slot || id != "sb-1788415903084-3817ea76" {
			t.Fatalf("roundtrip %q -> (%d,%q,%v)", name, s, id, ok)
		}
	}
	for name, why := range map[string]string{
		"fcns-sb-1788-aa": "the §1 spike's slotless form — stale by definition, but must not panic",
		"fcns-99999-x":    "slot out of range",
		"fcns-12-":        "empty sandbox id",
		"vh12":            "not ours at all",
	} {
		if _, _, ok := parseNetNSName(name); ok {
			t.Errorf("parseNetNSName(%q) accepted: %s", name, why)
		}
	}
}

func TestReclaimNetNSDecision(t *testing.T) {
	names := []string{
		"fcns-3-sb-alive",   // live VMM, keep
		"fcns-9-sb-dead",    // no VMM, remove
		"fcns-sb-spikeform", // unparseable, remove
		"cni-abcdef",        // not ours, untouched
	}
	keep, remove := reclaimNetNSDecision(names, map[string]bool{"sb-alive": true})
	if len(keep) != 1 || keep[0] != 3 {
		t.Fatalf("keepSlots = %v, want [3]", keep)
	}
	if len(remove) != 2 || remove[0] != "fcns-9-sb-dead" || remove[1] != "fcns-sb-spikeform" {
		t.Fatalf("remove = %v, want the dead and spike-form fcns names only", remove)
	}
}

func TestNetNSHostRulesetProtectsTheHost(t *testing.T) {
	rs := buildNetNSHostRuleset("eno1")
	for _, want := range []string{
		`ip saddr 10.128.0.0/16 oifname "eno1" masquerade`,
		"ip saddr 10.128.0.0/16 ct state established,related accept",
		`iifname "vh*" ip saddr 10.128.0.0/16 drop`, // a source-only drop kills the host's own local dials
		"ip saddr 10.128.0.0/16 ip daddr 10.128.0.0/16 drop",
		"ip saddr 10.128.0.0/16 ip daddr 100.64.0.0/10 drop", // the node may itself sit on a tailnet
	} {
		if !strings.Contains(rs, want) {
			t.Errorf("host ruleset lost %q:\n%s", want, rs)
		}
	}
	// The accept must precede the drop or the node's own envd dials lose their replies.
	if strings.Index(rs, "established,related accept") > strings.Index(rs, `iifname "vh*" ip saddr 10.128.0.0/16 drop`) {
		t.Error("reply-accept rendered after the drop — host-initiated envd dials would hang")
	}
	if !strings.Contains(buildNetNSHostRuleset(""), "ip saddr 10.128.0.0/16 masquerade") {
		t.Error("empty egress interface must still masquerade, not silently skip NAT")
	}
}

func TestHostRuleset6DropsBothDirectionsOnSandboxVeths(t *testing.T) {
	rs := buildNetNSHostRuleset6()
	for _, want := range []string{
		"table ip6 sandboxd_netns6",
		`iifname "vh*" drop`,
		`oifname "vh*" drop`,
	} {
		if !strings.Contains(rs, want) {
			t.Errorf("IPv6 backstop is missing %q — a guest that comes up with IPv6 anyway\n"+
				"reaches the host's veth link-local and anything bound on :: with no rule\n"+
				"in the path, because every other table here is `table ip` (v4 only):\n%s", want, rs)
		}
	}
	// Interface-keyed, not policy-keyed: this node does not own the host's IPv6 policy.
	if strings.Contains(rs, "policy drop") {
		t.Errorf("the backstop sets a drop POLICY, which takes the whole host's IPv6 with it,\n"+
			"not just sandbox veths:\n%s", rs)
	}
	if strings.Contains(rs, "masquerade") || strings.Contains(rs, "dnat") {
		t.Errorf("the backstop grew NAT; it is meant to be filter-only:\n%s", rs)
	}
}

// An ip6 match in an `ip` family table is an nft load error that takes every sandbox's egress down.
func TestIPv4TableStaysIPv4(t *testing.T) {
	for name, rs := range map[string]string{
		"in-ns": buildNetNSRuleset("vn7", "tap0", net.ParseIP("10.128.0.29"), net.ParseIP("10.128.0.30"), net.ParseIP("192.168.30.2"), "", "", false),
		"host":  buildNetNSHostRuleset("eno1"),
	} {
		if strings.Contains(rs, "ip6 ") {
			t.Errorf("%s ruleset is `table ip` but contains an ip6 match — nft refuses to load\n"+
				"the whole table, which takes egress down for every sandbox on this node:\n%s", name, rs)
		}
	}
}

// The host table's accept must be ahead of the slot-to-slot drop, and with an
// empty set it matches nothing — which is why adding it does not widen a node
// that has no tagged sandbox on it.
func TestHostRulesetPeerAcceptPrecedesTheDrop(t *testing.T) {
	rs := buildNetNSHostRuleset("eth0")
	i := strings.Index(rs, "ip saddr . ip daddr @peers accept")
	j := strings.Index(rs, "ip saddr 10.128.0.0/16 ip daddr 10.128.0.0/16 drop")
	if i < 0 || j < 0 || i > j {
		t.Fatalf("the peer accept must precede the slot-to-slot drop:\n%s", rs)
	}
	if !strings.Contains(rs, "set peers {") || !strings.Contains(rs, "type ipv4_addr . ipv4_addr") {
		t.Fatalf("the peer set is not declared, so the rule would not load:\n%s", rs)
	}
	// The input drop is untouched: a tag is not a route to the node.
	if !strings.Contains(rs, `iifname "vh*" ip saddr 10.128.0.0/16 drop`) {
		t.Fatalf("the host input drop is gone:\n%s", rs)
	}
}
