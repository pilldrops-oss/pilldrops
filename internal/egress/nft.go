package egress

import (
	"fmt"
	"strings"
)

// ChainOpts tunes the rendering. The egress chain is address allows, address
// denies, drop; the divert that hands a name-bearing connection to the name
// layer is NOT in it — see RenderDivertRule.
type ChainOpts struct {
	// ProxyPort is where a diverted connection is sent for name inspection.
	// Zero means no name layer on this node, so no divert rule is emitted and
	// a rule set naming a domain is refused (RenderEgressChainErr).
	ProxyPort int
	// IifName is the guest-facing interface inside the namespace. The divert is
	// keyed on it so traffic the node sends INTO the guest — previews, envd —
	// is never diverted into the proxy. Empty emits no divert.
	IifName string
	// ResolverAddrs are the guest's baked-in nameservers. A policy in force
	// replaces the chain's default tail, so without these DNS stops resolving
	// and an address allow list looks broken for anything named. Scoped to
	// those addresses rather than opening UDP 53 to the world, which is a
	// tunnel out of an otherwise closed sandbox.
	ResolverAddrs []string
}

// RenderEgressChain returns the policy rules for the in-namespace egress
// chain, in order, without the platform drops or the chain's braces. A nil
// rule set returns nil so the caller keeps the unrestricted chain.
func RenderEgressChain(allow, deny []Entry, opts ChainOpts) []string {
	if allow == nil && deny == nil {
		return nil
	}

	var out []string
	// Only when the policy permits something. A rule set that allows nothing is
	// "this sandbox has no network", and DNS is network.
	if len(allow) > 0 {
		for _, a := range opts.ResolverAddrs {
			out = append(out, fmt.Sprintf("udp dport 53 ip daddr %s accept", a))
		}
	}

	// 1. Address allow entries.
	for _, e := range allow {
		if e.Kind == KindAddr {
			out = append(out, fmt.Sprintf("ip daddr %s accept", e.Prefix))
		}
	}

	// 2. Domain entries are decided by the name layer, and its divert is a
	// `redirect` — a nat statement, which nft refuses in this filter chain and
	// which fails the load of the whole table if emitted here. It lives in the
	// namespace's prerouting chain instead (RenderDivertRule), an earlier hook
	// than this one, so it still precedes these address denials: with "deny
	// 0.0.0.0/0, allow *.github.com" the packet must reach userspace before the
	// blanket deny, or its SNI is never read.
	if (hasDomain(allow) || hasDomain(deny)) && opts.ProxyPort <= 0 {
		// Without a name layer the domain entries would silently drop;
		// leave a marker the caller must not ignore (see RenderEgressChainErr).
		out = append(out, "# NAME LAYER UNAVAILABLE: domain entries in this rule set cannot be enforced")
	}

	// 3. Address deny entries.
	for _, e := range deny {
		if e.Kind == KindAddr {
			out = append(out, fmt.Sprintf("ip daddr %s drop", e.Prefix))
		}
	}

	// 4. Default deny.
	out = append(out, "drop")
	return out
}

// RenderEgressChainErr is RenderEgressChain that refuses domain entries with
// no name layer. Split out because the renderer also shows an operator what a
// policy would become, where a missing proxy is worth displaying.
func RenderEgressChainErr(allow, deny []Entry, opts ChainOpts) ([]string, error) {
	lines := RenderEgressChain(allow, deny, opts)
	if (hasDomain(allow) || hasDomain(deny)) && opts.ProxyPort <= 0 {
		return nil, fmt.Errorf("this rule set has domain entries and no name layer is available "+
			"to evaluate them; applying it would drop that traffic while the policy reads as "+
			"permitting it (%d domain entries)", countDomain(allow)+countDomain(deny))
	}
	return lines, nil
}

// RenderDivertRule returns the namespace's prerouting nat rules that hand
// name-bearing connections to the name layer, in order. nil when the rule set
// names no domain, when this node has no name layer, or when the caller did
// not say which interface the guest is on — in each case nothing is diverted
// and the egress chain decides everything on address alone.
//
// prerouting runs before the forward hook, so a redirect here bypasses the
// platform drops in the egress chain. That is why the reserved ranges return
// first: without them a guest reaches the metadata service by putting a
// permitted name in a Host header. Addresses the policy already allows return
// too — Match puts allow first, so the verdict is the same and the userspace
// hop buys nothing.
func RenderDivertRule(allow, deny []Entry, opts ChainOpts) []string {
	if opts.ProxyPort <= 0 || opts.IifName == "" || !(hasDomain(allow) || hasDomain(deny)) {
		return nil
	}
	out := make([]string, 0, len(reserved)+len(allow)+1)
	for _, p := range reserved {
		out = append(out, fmt.Sprintf("iifname %q ip daddr %s return", opts.IifName, p))
	}
	for _, e := range allow {
		if e.Kind == KindAddr {
			out = append(out, fmt.Sprintf("iifname %q ip daddr %s return", opts.IifName, e.Prefix))
		}
	}
	return append(out, fmt.Sprintf("iifname %q tcp dport { %d, %d } redirect to :%d",
		opts.IifName, PortHTTP, PortHTTPS, opts.ProxyPort))
}

func hasDomain(list []Entry) bool { return countDomain(list) > 0 }

func countDomain(list []Entry) int {
	n := 0
	for _, e := range list {
		if e.Kind == KindDomain {
			n++
		}
	}
	return n
}

// ChainText joins rendered rules for embedding in an nft ruleset, indented to
// match buildNetNSRuleset's existing body.
func ChainText(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return "\t\t" + strings.Join(lines, "\n\t\t")
}
