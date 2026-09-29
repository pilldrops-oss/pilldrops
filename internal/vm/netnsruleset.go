package vm

// The in-namespace nftables ruleset and the tenant egress policy spliced into
// it. Deliberately NOT in netns_linux.go: that file is //go:build linux and
// runs zero tests on a macOS checkout, so a rule rendered there is asserted
// nowhere most development happens. Nothing here touches netlink or nft.

import (
	"fmt"
	"net"

	"github.com/pilldrops-oss/pilldrops/internal/egress"
)

// netnsHostTable is the node-owned host-side nft table for netns mode. Here
// rather than in netns_linux.go so the rendering that names it is asserted on
// a macOS checkout too.
const netnsHostTable = "sandboxd_netns"

// guestResolvers are the nameservers baked into every template's /etc/resolv.conf
// by internal/templatebuild. A sandbox under a policy can still resolve names
// through these and nothing else; change them in one place and the other breaks.
var guestResolvers = []string{"8.8.8.8", "1.1.1.1"}

// egressProxyPort is where the name layer listens INSIDE a sandbox's
// namespace, on the tap gateway. Namespace-local, so the only thing it can
// collide with is this daemon's own in-namespace resolver on 53.
const egressProxyPort = 18443

// renderEgressPolicy turns a sandbox's rule set into two pieces of ruleset: the
// body of the egress chain, and the prerouting rules that hand a name-bearing
// connection to the name layer. Both "" means no policy, and the chain keeps
// its open default.
//
// The proxy port is passed unconditionally because every caller of this is a
// netns path and every netns path starts the name layer for a rule set that
// needs one (startNameLayer). A node that could not would have to pass 0 here,
// and RenderEgressChainErr would then refuse the domain entries rather than
// load a chain that drops the traffic the policy says it permits.
func renderEgressPolicy(rules *egress.Rules, tapName string) (chain, divert string, err error) {
	allow, deny, err := rules.Parse()
	if err != nil {
		return "", "", fmt.Errorf("egress rules: %w", err)
	}
	opts := egress.ChainOpts{ProxyPort: egressProxyPort, IifName: tapName, ResolverAddrs: guestResolvers}
	lines, err := egress.RenderEgressChainErr(allow, deny, opts)
	if err != nil {
		return "", "", fmt.Errorf("egress rules: %w", err)
	}
	divert = egress.ChainText(egress.RenderDivertRule(allow, deny, opts))
	if divert != "" {
		divert += "\n"
	}
	return egress.ChainText(lines), divert, nil
}

// buildNetNSRuleset renders the in-namespace nftables ruleset: DNAT node traffic
// to the guest, masquerade guest egress, and drop everything not allow-listed.
// The 10.0.0.0/8 drop is the inter-slot isolation; other slots live in 10.128/16.
// policy, when non-empty, replaces the open default tail of the egress chain;
// the platform drops above it are not negotiable and stay first either way.
// hostIP is the slot's host veth end (slotIP(slot, 1)): with
// PILLDROPS_LOCAL_API_PORT set, that address on that port is accepted ahead
// of the 10/8 drop so a guest can reach a control plane on this host.
// tagged adds the one line a sandbox on a network needs; false renders exactly
// what this function rendered before tag networks existed.
// divert, when non-empty, hands the guest's name-bearing connections to the
// name layer. It goes in prerouting, after the node's DNAT: `redirect` is a nat
// statement that nft refuses in the egress chain, and prerouting is an earlier
// hook anyway, so a diverted packet is judged on its name before any address
// denial in the chain below can kill it.
func buildNetNSRuleset(vn, tapName string, hostIP, nsIP, guestIP net.IP, policy, divert string, tagged bool) string {
	tail := policy
	if tail == "" {
		tail = "\t\tudp dport 53 accept\n\t\ttcp dport { 53, 80, 443 } accept\n\t\tdrop"
	}
	return fmt.Sprintf(`flush ruleset
table ip sandbox {
	chain prerouting {
		type nat hook prerouting priority dstnat;
		iifname %q ip daddr %s dnat to %s
%s%s	}
	chain postrouting {
		type nat hook postrouting priority srcnat;
		oifname %q ip saddr 192.168.0.0/16 masquerade
	}
	chain forward {
		type filter hook forward priority filter; policy drop;
		ct state established,related accept
		iifname %q oifname %q accept
		iifname %q jump egress
	}
	chain egress {
		ip daddr 169.254.0.0/16 drop
%s%s		ip daddr 10.0.0.0/8 drop
		ip daddr 172.16.0.0/12 drop
		ip daddr 100.64.0.0/10 drop
%s
	}
}
`, vn, nsIP, guestIP, taggedDNSRedirect(tagged, tapName), divert,
		vn, vn, tapName, tapName, localAPIEgressRule(hostIP), taggedEgressRule(tagged), tail)
}

// netnsSupernet is the slot supernet every 10.128 literal in this package
// means: slotIP packs consecutive /30s out of it (TestSlotAddressing ties the
// two together), the host table keys its drops on it, and the base-chain
// accepts below name it. One string, so a change cannot land in half the rules.
const netnsSupernet = "10.128.0.0/16"

// netnsBaseChainRules are the iptables accepts that let sandbox traffic survive
// the *base* filter FORWARD chain. That chain is a different netfilter hook
// from the sandboxd_netns nft table, and installing docker sets its policy to
// DROP — which is every node scripts/provision-node.sh touches, because docker
// is what builds the template rootfs images. A drop there kills sandbox egress
// in both directions no matter how correct sandboxd_netns is, and the symptom
// is a sandbox with no outbound network at all, not a DNS failure.
//
// The other half of the asymmetry: scripts/setup-host.sh installs the matching
// pair for the bridge path (192.168.0.0/16 after iptables masks it) and nothing
// named the namespace subnet, which is the default path (FC_NETNS defaults on).
//
// This widens nothing. An ACCEPT here ends traversal of the `filter` table
// only; the sandboxd_netns base chain runs on the same hook and is still the
// arbiter of slot-to-slot and metadata traffic, and the in-namespace ruleset —
// the primary egress control, with its RFC1918/CGNAT/link-local drops — is
// upstream of both. Nothing but a sandbox veth can source the slot supernet.
//
// Inbound is conntrack-scoped rather than blanket, mirroring setup-host.sh's
// `-d 192.168.0.0/16 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT`:
// replies to sandbox-initiated connections, not new connections from the world.
//
// The outbound rule supersedes the tag-network peer exception that used to live
// in tagnet_linux.go (`-s 10.128/16 -d 10.128/16 ACCEPT`): it was the same fix
// for the same chain, scoped to the one case someone had measured. Do not add
// it back alongside these.
func netnsBaseChainRules(chain string) [][]string {
	return [][]string{
		{chain, "-s", netnsSupernet, "-j", "ACCEPT"},
		{chain, "-d", netnsSupernet, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
	}
}
