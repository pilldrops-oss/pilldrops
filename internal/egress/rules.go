// Package egress is the sandbox egress rule set and the matcher that decides
// it. Everything here is pure: no sockets, no netns, no nftables, no Docker,
// so every front end decides a connection the same way.
package egress

import (
	"fmt"
	"net/netip"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// Rule-set size limits, matching what comparable providers publish.
const (
	MaxAddrEntries   = 10
	MaxDomainEntries = 20
)

// Ports where a name appears on the wire: TLS carries SNI and HTTP carries
// Host. Nothing else can be matched by name.
const (
	PortHTTP  = 80
	PortHTTPS = 443
)

// NameIsOnTheWire reports whether a connection to port carries a hostname the
// matcher could read.
func NameIsOnTheWire(port int) bool { return port == PortHTTP || port == PortHTTPS }

// Rules is one sandbox's egress policy. nil means unrestricted; a non-nil value
// whose lists permit nothing denies everything. The two must never collapse,
// or locked-down sandboxes fail open. Block all egress with Deny: ["0.0.0.0/0"].
type Rules struct {
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

// AllTraffic is the entry that matches every IPv4 destination.
const AllTraffic = "0.0.0.0/0"

// Kind classifies one rule entry.
type Kind int

const (
	KindInvalid Kind = iota
	// KindDomain is a hostname, optionally prefixed "*." to cover subdomains.
	KindDomain
	// KindAddr is an IPv4 address or CIDR; a bare address is normalised to /32.
	KindAddr
)

// Entry is one parsed, validated rule.
type Entry struct {
	Kind   Kind
	Raw    string
	Prefix netip.Prefix // KindAddr only
	Domain string       // KindDomain only, lower-cased, no trailing dot
	// Wildcard is true for "*.example.com": subdomains of Domain, not Domain itself.
	Wildcard bool
}

// ParseEntry classifies and validates one rule entry. IPv6 is refused because
// the guest has no IPv6 stack and every packet rule on the node is IPv4, so an
// accepted v6 entry would never be evaluated.
func ParseEntry(raw string) (Entry, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Entry{}, fmt.Errorf("empty rule entry")
	}

	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return Entry{}, fmt.Errorf("%q is not a valid CIDR block: %w", raw, err)
		}
		if !p.Addr().Is4() {
			return Entry{}, fmt.Errorf("%q is IPv6; sandboxes have no IPv6 stack and every packet rule on the node is IPv4, so this entry would never be evaluated", raw)
		}
		return Entry{Kind: KindAddr, Raw: raw, Prefix: p.Masked()}, nil
	}

	if a, err := netip.ParseAddr(s); err == nil {
		if !a.Is4() {
			return Entry{}, fmt.Errorf("%q is IPv6; sandboxes have no IPv6 stack and every packet rule on the node is IPv4, so this entry would never be evaluated", raw)
		}
		return Entry{Kind: KindAddr, Raw: raw, Prefix: netip.PrefixFrom(a, 32)}, nil
	}

	return parseDomain(raw, s)
}

func parseDomain(raw, s string) (Entry, error) {
	e := Entry{Kind: KindDomain, Raw: raw}
	d := strings.ToLower(strings.TrimSuffix(s, "."))
	if after, ok := strings.CutPrefix(d, "*."); ok {
		e.Wildcard, d = true, after
	}
	if d == "" {
		return Entry{}, fmt.Errorf("%q has no domain after the wildcard", raw)
	}
	// A second "*" is refused rather than treated as a literal, since the entry would match narrower than it reads.
	if strings.Contains(d, "*") {
		return Entry{}, fmt.Errorf("%q: a wildcard is only supported as a leading \"*.\" covering subdomains", raw)
	}
	if strings.Contains(d, "/") || strings.Contains(d, ":") || strings.ContainsAny(d, " \t") {
		return Entry{}, fmt.Errorf("%q must be a bare hostname — no scheme, port or path", raw)
	}
	if !strings.Contains(d, ".") {
		return Entry{}, fmt.Errorf("%q is not a fully qualified domain name", raw)
	}
	// "999.1.1.1" fails netip.ParseAddr and would otherwise be accepted as a
	// domain that can never match. No TLD is all-digits (RFC 3696), so that is the check.
	if last := d[strings.LastIndex(d, ".")+1:]; last == "" || strings.IndexFunc(last, isNotDigit) < 0 {
		return Entry{}, fmt.Errorf("%q looks like a malformed IP address, not a hostname "+
			"(no top-level domain is all-digits) — as a domain rule it would never match anything", raw)
	}
	e.Domain = d
	return e, nil
}

func isNotDigit(r rune) bool { return r < '0' || r > '9' }

// Parse validates a whole rule set and returns the parsed entries. Both lists
// are checked so a caller sees every problem at once.
func (r *Rules) Parse() (allow, deny []Entry, err error) {
	if r == nil {
		return nil, nil, nil
	}
	var errs []string
	allow, aerr := parseList(r.Allow, "allow")
	if aerr != nil {
		errs = append(errs, aerr.Error())
	}
	deny, derr := parseList(r.Deny, "deny")
	if derr != nil {
		errs = append(errs, derr.Error())
	}
	if len(errs) > 0 {
		return nil, nil, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return allow, deny, nil
}

// reserved are the IPv4 ranges an allow entry may not name or cover. Allowing
// one would either widen what the platform grants a sandbox (0.0.0.0/0 accepts
// every port, where the default chain permits 53/80/443) or hand it a route to
// the node, another sandbox, or the metadata service. A DENY may name anything:
// "deny 0.0.0.0/0" is how a policy says "no network".
var reserved = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

// PublicDest reports whether addr is a destination a sandbox may be connected
// to. Exported for the name layer, which dials an address the rules never
// named — the one a permitted hostname resolved to — from outside the
// namespace, where the in-ns platform drops that protect the node, the other
// slots and the metadata service no longer apply. Same list as publicOnly, so
// an allow entry and a resolved address are held to one standard.
//
// Non-IPv4 is refused for the reason ParseEntry gives: the guest has no IPv6
// stack and every packet rule on the node is IPv4.
func PublicDest(addr netip.Addr) bool {
	if !addr.Is4() {
		return false
	}
	return publicOnly(netip.PrefixFrom(addr, 32)) == nil
}

// ReservedPrefixes lists the ranges no sandbox connection may be diverted past
// the kernel for. The name layer's divert rule names them so a redirect in
// prerouting — which runs before the forward hook the platform drops live on —
// cannot become a route to the node or the metadata service.
func ReservedPrefixes() []netip.Prefix { return append([]netip.Prefix(nil), reserved...) }

// publicOnly refuses an allow entry that names or covers a reserved range.
func publicOnly(p netip.Prefix) error {
	for _, r := range reserved {
		if p.Overlaps(r) {
			return fmt.Errorf("%s covers %s, which is not a public destination; allowing it would "+
				"open the node, another sandbox, the metadata service, or every port to the whole "+
				"internet — name the exact public address or range instead", p, r)
		}
	}
	return nil
}

func parseList(list []string, which string) ([]Entry, error) {
	out := make([]Entry, 0, len(list))
	var domains, addrs int
	for _, raw := range list {
		e, err := ParseEntry(raw)
		if err != nil {
			return nil, fmt.Errorf("%s list: %w", which, err)
		}
		if which == "allow" && e.Kind == KindAddr {
			if err := publicOnly(e.Prefix); err != nil {
				return nil, fmt.Errorf("allow list: %w", err)
			}
		}
		switch e.Kind {
		case KindDomain:
			domains++
		case KindAddr:
			addrs++
		}
		out = append(out, e)
	}
	if domains > MaxDomainEntries {
		return nil, fmt.Errorf("%s list has %d domain entries, limit is %d", which, domains, MaxDomainEntries)
	}
	if addrs > MaxAddrEntries {
		return nil, fmt.Errorf("%s list has %d address/CIDR entries, limit is %d", which, addrs, MaxAddrEntries)
	}
	return out, nil
}

// HasDomains reports whether the rule set names any domain, which is the half
// that cannot be decided in the kernel and needs the name layer.
func (r *Rules) HasDomains() bool {
	if r == nil {
		return false
	}
	for _, raw := range append(append([]string{}, r.Allow...), r.Deny...) {
		if e, err := ParseEntry(raw); err == nil && e.Kind == KindDomain {
			return true
		}
	}
	return false
}

// Validate reports whether the rule set is usable as written.
func (r *Rules) Validate() error {
	_, _, err := r.Parse()
	return err
}

// CheckWildcards refuses an allow wildcard over a public suffix ("*.co.uk",
// "*.github.io"), which admits every site registered under it. Separate from
// ParseEntry so it applies to rules as they arrive, not to ones already stored.
func CheckWildcards(r *Rules) error {
	if r == nil {
		return nil
	}
	for _, raw := range r.Allow {
		e, err := ParseEntry(raw)
		if err != nil || !e.Wildcard {
			continue
		}
		if suffix, _ := publicsuffix.PublicSuffix(e.Domain); suffix == e.Domain {
			return fmt.Errorf("allow list: %q covers the public suffix %q, so it would allow every site "+
				"registered under it; name the sites themselves, e.g. \"*.example.%s\"", raw, e.Domain, e.Domain)
		}
	}
	return nil
}
