package vm

import (
	"net"
	"strings"
	"testing"
)

// untaggedGolden is what buildNetNSRuleset rendered for an untagged sandbox
// before tag networks existed, copied byte for byte off main. This is the
// cheapest proof that opting in did not widen the default: an untagged
// sandbox's kernel rules are the same text they always were, so every
// block.peer.* probe in the security suite is measuring the same thing.
const untaggedGolden = `flush ruleset
table ip sandbox {
	chain prerouting {
		type nat hook prerouting priority dstnat;
		iifname "vn7" ip daddr 10.128.0.30 dnat to 192.168.30.2
	}
	chain postrouting {
		type nat hook postrouting priority srcnat;
		oifname "vn7" ip saddr 192.168.0.0/16 masquerade
	}
	chain forward {
		type filter hook forward priority filter; policy drop;
		ct state established,related accept
		iifname "vn7" oifname "tap0" accept
		iifname "tap0" jump egress
	}
	chain egress {
		ip daddr 169.254.0.0/16 drop
		ip daddr 10.0.0.0/8 drop
		ip daddr 172.16.0.0/12 drop
		ip daddr 100.64.0.0/10 drop
		udp dport 53 accept
		tcp dport { 53, 80, 443 } accept
		drop
	}
}
`

func TestUntaggedRulesetIsUnchanged(t *testing.T) {
	t.Setenv("PILLDROPS_LOCAL_API_PORT", "")
	host, ns, guest := net.ParseIP("10.128.0.29"), net.ParseIP("10.128.0.30"), net.ParseIP("192.168.30.2")
	got := buildNetNSRuleset("vn7", "tap0", host, ns, guest, "", "", false)
	if got != untaggedGolden {
		t.Fatalf("an untagged sandbox's ruleset changed.\n--- got ---\n%s\n--- want ---\n%s", got, untaggedGolden)
	}
}

// Tagged adds exactly one line, and it must sit ahead of the 10/8 drop or the
// namespace kills the traffic the host chain has just been told to admit.
func TestTaggedRulesetAddsOneLineAheadOfTheDrop(t *testing.T) {
	t.Setenv("PILLDROPS_LOCAL_API_PORT", "")
	host, ns, guest := net.ParseIP("10.128.0.29"), net.ParseIP("10.128.0.30"), net.ParseIP("192.168.30.2")
	rs := buildNetNSRuleset("vn7", "tap0", host, ns, guest, "", "", true)
	const rule = "ip daddr 10.128.0.0/16 accept"
	i, j := strings.Index(rs, rule), strings.Index(rs, "ip daddr 10.0.0.0/8 drop")
	if i < 0 || j < 0 || i > j {
		t.Fatalf("the supernet accept must precede the 10/8 drop:\n%s", rs)
	}
	const redirect = `iifname "tap0" udp dport 53 redirect to :53`
	stripped := strings.Replace(rs, "\t\t"+rule+"\n", "", 1)
	stripped = strings.Replace(stripped, "\t\t"+redirect+"\n", "", 1)
	if stripped != untaggedGolden {
		t.Fatalf("tagging changed more than the accept and the redirect:\n%s", stripped)
	}
	// The redirect belongs to the nat prerouting chain and matches only traffic
	// arriving from the guest's tap, never from the host veth.
	if strings.Index(rs, redirect) > strings.Index(rs, "chain postrouting") {
		t.Fatalf("the DNS redirect is not in the prerouting chain:\n%s", rs)
	}
	// The metadata drop still comes first: a tag is not a way to the node.
	if strings.Index(rs, "169.254.0.0/16 drop") > i {
		t.Fatal("the supernet accept precedes the metadata drop")
	}
}

// The peer set is the whole membership, re-rendered: every pair both ways, no
// self-pairs, nothing for a sandbox that shares no tag, and sorted so the
// script is stable.
func TestRenderPeerSet(t *testing.T) {
	members := map[string]tagMember{
		"a": {sets: []string{"o/t"}, ip: "10.128.0.2"},
		"b": {sets: []string{"o/t", "o/u"}, ip: "10.128.0.6"},
		"c": {sets: []string{"o/u"}, ip: "10.128.0.10"},
		"d": {sets: nil, ip: "10.128.0.14"},                 // untagged
		"e": {sets: []string{"other/t"}, ip: "10.128.0.18"}, // same word, another org
	}
	got := renderPeerSet(members)
	const decl = "add set ip sandboxd_netns peers { type ipv4_addr . ipv4_addr; }\n"
	want := decl + "flush set ip sandboxd_netns peers\n" +
		"add element ip sandboxd_netns peers { " +
		"10.128.0.10 . 10.128.0.6, " +
		"10.128.0.2 . 10.128.0.6, " +
		"10.128.0.6 . 10.128.0.10, " +
		"10.128.0.6 . 10.128.0.2 }\n"
	if got != want {
		t.Fatalf("renderPeerSet =\n%s\nwant\n%s", got, want)
	}
	// The untagged sandbox and the other org's appear in no pair at all.
	for _, ip := range []string{"10.128.0.14", "10.128.0.18"} {
		if strings.Contains(got, ip) {
			t.Errorf("%s is in the peer set; it shares no network with anyone", ip)
		}
	}
	// A membership with nothing in it flushes and adds nothing, which is what
	// closes the last tagged sandbox off again.
	if got := renderPeerSet(map[string]tagMember{"d": {ip: "10.128.0.14"}}); got != decl+"flush set ip sandboxd_netns peers\n" {
		t.Fatalf("empty membership rendered %q", got)
	}
}

func TestPeersOf(t *testing.T) {
	members := map[string]tagMember{
		"a": {sets: []string{"o/t"}, ip: "10.128.0.2", slug: "alpha"},
		"b": {sets: []string{"o/t"}, ip: "10.128.0.6", slug: "bravo"},
		"c": {sets: []string{"o/u"}, ip: "10.128.0.10", slug: "charlie"},
		"n": {sets: []string{"o/t"}, ip: "10.128.0.14"}, // no slug, no name to answer
	}
	got := peersOf(members, "a")
	if len(got) != 1 || got["bravo"] != "10.128.0.6" {
		t.Fatalf("peersOf(a) = %v, want only bravo", got)
	}
	if got := peersOf(members, "c"); len(got) != 0 {
		t.Fatalf("peersOf(c) = %v, want nothing: it shares no network", got)
	}
	// An untagged sandbox resolves nobody, which is what makes .pd NXDOMAIN for it.
	if got := peersOf(members, "nope"); got != nil {
		t.Fatalf("peersOf(unknown) = %v, want nil", got)
	}
}
