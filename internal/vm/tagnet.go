package vm

// Tag networks: the opt-in half of sandbox-to-sandbox reachability. The default
// is unchanged and stays measured — the host's slot-to-slot drop and each
// namespace's 10.0.0.0/8 drop are still there, and an untagged sandbox renders
// byte-identical rules to a node without this file (TestUntaggedRulesetIsUnchanged).
//
// Nothing here touches netlink or nft; the rendering is asserted on a macOS
// checkout, where netns_linux.go compiles for nobody.

import (
	"fmt"
	"sort"
	"strings"
)

// peerSetName is the one named set in the host table. It holds ordered pairs of
// slot addresses, not per-tag membership: a set per tag would need the tags
// remembered to delete from on teardown, and a forgotten delete leaves a live
// element pointing at an address the next sandbox will be given. Pairs let the
// whole set be re-rendered from the current membership, so a stale element
// cannot exist by construction.
//
// ponytail: O(n²) elements per tag. A tag of 50 sandboxes is 2450 elements,
// which nft handles; if a tag ever grows past a few hundred, key the rule on a
// per-tag mark instead.
const peerSetName = "peers"

// tagMember is one sandbox's membership as the host rules need it.
type tagMember struct {
	// sets are the org-scoped network keys; empty means the sandbox is on no
	// network and appears in no pair.
	sets []string
	// slug is what peers resolve it by; "" means it has no name yet.
	slug string
	// ip is the slot address peers reach it on (slotIP(slot, 2)).
	ip string
}

// sharesSet reports whether two memberships have a network in common.
func sharesSet(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

// renderPeerSet returns the nft script that makes the host's peer set exactly
// the pairs this membership implies. flush plus adds in one file, so it loads
// atomically: there is no instant in which a sandbox has lost a peer it should
// keep. An empty membership renders the flush alone, which is what closes the
// last tagged sandbox off again.
func renderPeerSet(members map[string]tagMember) string {
	var pairs []string
	for aID, a := range members {
		if len(a.sets) == 0 || a.ip == "" {
			continue
		}
		for bID, b := range members {
			// Both directions, since nft matches saddr . daddr in one order,
			// and never a sandbox with itself: its own address is its
			// namespace's, and reaching it is not a peer relationship.
			if aID == bID || len(b.sets) == 0 || b.ip == "" || !sharesSet(a.sets, b.sets) {
				continue
			}
			pairs = append(pairs, a.ip+" . "+b.ip)
		}
	}
	sort.Strings(pairs)
	// `add set` is a no-op when it exists and re-declares it when it does not:
	// the host table is node-global and a second netns daemon starting on the
	// same box recreates it, taking the set with it.
	// ponytail: this heals the set, not the accept rule that reads it, so a
	// clobbered table fails closed (peers unreachable) until this daemon
	// re-runs EnsureNetNSHostRules. One netns daemon per node is the real fix.
	out := fmt.Sprintf("add set ip %s %s { type ipv4_addr . ipv4_addr; }\n", netnsHostTable, peerSetName)
	out += fmt.Sprintf("flush set ip %s %s\n", netnsHostTable, peerSetName)
	if len(pairs) > 0 {
		out += fmt.Sprintf("add element ip %s %s { %s }\n", netnsHostTable, peerSetName, strings.Join(pairs, ", "))
	}
	return out
}

// peersOf returns the slug-to-address map a sandbox's own resolver may answer:
// the sandboxes that share at least one of its networks, on this node. Excludes
// the caller itself and anything with no slug, which has no name to answer.
func peersOf(members map[string]tagMember, id string) map[string]string {
	self, ok := members[id]
	if !ok || len(self.sets) == 0 {
		return nil
	}
	out := map[string]string{}
	for otherID, m := range members {
		if otherID == id || m.slug == "" || m.ip == "" || !sharesSet(self.sets, m.sets) {
			continue
		}
		out[m.slug] = m.ip
	}
	return out
}

// taggedEgressRule is the one line a tagged sandbox's egress chain gains, ahead
// of the 10.0.0.0/8 drop that is the in-namespace half of peer isolation. It
// widens the namespace to the whole slot supernet rather than to named peers,
// and is safe to do so because the host chains decide who is actually reachable
// there: the host input chain still drops every new connection to the node's
// own veth addresses, and the host forward chain still drops slot-to-slot
// traffic that is not a pair in @peers. An untagged sandbox gets "" and its
// ruleset is unchanged.
func taggedEgressRule(tagged bool) string {
	if !tagged {
		return ""
	}
	return "\t\tip daddr 10.128.0.0/16 accept\n"
}

// taggedDNSRedirect sends a tagged guest's own DNS at the resolver this
// namespace runs, whatever its /etc/resolv.conf says. Redirecting rather than
// rewriting resolv.conf means no guest-side change, no template rebake, and an
// app that hard-codes 8.8.8.8 is answered the same way. It is a local delivery,
// so it is not subject to the egress chain and a sandbox under a deny-all
// policy still resolves its peers.
//
// ponytail: UDP only. A .pd lookup that fell back to TCP would go upstream and
// NXDOMAIN; answers here are one A record and never set TC, so nothing does.
func taggedDNSRedirect(tagged bool, tapName string) string {
	if !tagged {
		return ""
	}
	return "\t\tiifname \"" + tapName + "\" udp dport 53 redirect to :53\n"
}
