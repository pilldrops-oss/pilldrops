package egress

import (
	"fmt"
	"sort"
	"strings"
)

// Narrow composes a base policy with a requested one so a request may narrow
// its egress, never widen it: allow is intersected and deny is unioned.
//
// ponytail: entry-equality intersection, not subset algebra. Upgrade to real
// containment (netip.Prefix.Overlaps for addresses, suffix containment for
// domains) if callers actually hit this.
func Narrow(base, req *Rules) *Rules {
	if base == nil {
		return req
	}
	if req == nil {
		return base
	}
	return &Rules{
		Allow: intersect(base.Allow, req.Allow),
		Deny:  union(base.Deny, req.Deny),
	}
}

// intersect keeps the entries present in both lists, compared normalised.
func intersect(a, b []string) []string {
	inB := make(map[string]struct{}, len(b))
	for _, s := range b {
		inB[normalise(s)] = struct{}{}
	}
	out := []string{}
	for _, s := range a {
		if _, ok := inB[normalise(s)]; ok {
			out = append(out, s)
		}
	}
	return out
}

// union concatenates and de-duplicates, keeping the base list's order first.
func union(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := []string{}
	for _, s := range append(append([]string{}, a...), b...) {
		k := normalise(s)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, s)
	}
	return out
}

// normalise is the comparison key for entry equality, mirroring ParseEntry's
// canonical form. An entry that does not parse compares on its trimmed text;
// Validate rejects it separately.
func normalise(s string) string {
	if e, err := ParseEntry(s); err == nil {
		switch e.Kind {
		case KindDomain:
			if e.Wildcard {
				return "*." + e.Domain
			}
			return e.Domain
		case KindAddr:
			return e.Prefix.String()
		}
	}
	return strings.ToLower(strings.TrimSpace(s))
}

// DenyAll is the rule set that permits nothing; service templates default to
// it. The explicit deny entry, rather than &Rules{}, keeps a resolved policy readable.
func DenyAll() *Rules { return &Rules{Deny: []string{AllTraffic}} }

// Advisories reports configurations that are legal and probably not what the
// author expected, such as an allow list of only domains, which a database
// port can never match. Returned on create and update; not an error.
func (r *Rules) Advisories() []string {
	if r == nil {
		return nil
	}
	allow, _, err := r.Parse()
	if err != nil {
		return nil // Validate reports this properly; do not double-report.
	}
	var out []string

	var domains, addrs int
	for _, e := range allow {
		switch e.Kind {
		case KindDomain:
			domains++
		case KindAddr:
			addrs++
		}
	}
	if domains > 0 && addrs == 0 {
		out = append(out, fmt.Sprintf(
			"the allow list contains %d domain entries and no address entries: domains are matched "+
				"by TLS SNI on port %d and the HTTP Host header on port %d, so traffic on every "+
				"other port (databases, SSH, raw TCP, UDP) is denied regardless of these entries. "+
				"Add an IP or CIDR entry if you need those.",
			domains, PortHTTPS, PortHTTP))
	}
	if len(allow) == 0 && len(r.Deny) > 0 {
		out = append(out, "the allow list is empty, so this policy denies all egress; "+
			"the deny entries have nothing left to exclude")
	}
	sort.Strings(out)
	return out
}
