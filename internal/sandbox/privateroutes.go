package sandbox

import (
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

// PrivateRoutes are the subnet and split-DNS grants a node routes for one
// sandbox. Subnets are CIDRs with the platform's own ranges already removed.
type PrivateRoutes struct {
	Subnets []string `json:"subnets"`
	DNS     []string `json:"dns"`
}

// Empty reports that the routes reach nothing.
func (r PrivateRoutes) Empty() bool { return len(r.Subnets) == 0 && len(r.DNS) == 0 }

// PrivateDNSAnswer is one record of a relayed split-DNS answer, in chain order.
type PrivateDNSAnswer struct {
	Type  string `json:"type"` // "A" or "CNAME"
	Value string `json:"value"`
	TTL   uint32 `json:"ttl"`
}

// PrivateDNSReply is the control plane's answer to one relayed lookup.
type PrivateDNSReply struct {
	RCode   int                `json:"rcode"`
	Answers []PrivateDNSAnswer `json:"answers"`
}

const (
	MaxConnectorSubnets  = 20
	MaxConnectorSuffixes = 10
	MaxEffectiveGrants   = 50
	// MaxRoutedPrefixes bounds the carved list a node accepts: carving one /8
	// around the platform ranges leaves at most a few dozen prefixes.
	MaxRoutedPrefixes = 1024
)

// PlatformRanges are never routed through a connector: slot addresses, the
// guest link and private-target range, link-local, CGNAT and loopback.
var PlatformRanges = []netip.Prefix{
	netip.MustParsePrefix("10.128.0.0/16"),
	netip.MustParsePrefix("192.168.30.0/23"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
}

// ParsePrivateSubnet parses an announced or granted subnet: IPv4, /8 to /32,
// with no host bits set.
func ParsePrivateSubnet(s string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(strings.TrimSpace(s))
	if err != nil || !p.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("subnet %q: want an IPv4 CIDR like 10.0.0.0/16", s)
	}
	if p.Bits() == 0 {
		return netip.Prefix{}, fmt.Errorf("subnet %q would route every address; that is not a subnet route", s)
	}
	if p.Bits() < 8 {
		return netip.Prefix{}, fmt.Errorf("subnet %q: at most a /8", s)
	}
	if p.Masked() != p {
		return netip.Prefix{}, fmt.Errorf("subnet %q has host bits set; did you mean %s?", s, p.Masked())
	}
	return p, nil
}

// ValidPrivateSuffix checks a split-DNS suffix's shape: two or more lowercase
// DNS labels outside the sandbox-internal zones. The control plane also
// refuses platform domains and public suffixes.
func ValidPrivateSuffix(s string) error {
	if s == "" || s != strings.ToLower(s) || strings.HasSuffix(s, ".") || len(s) > 253 {
		return fmt.Errorf("dns suffix %q: want lowercase labels with no trailing dot", s)
	}
	if _, err := netip.ParseAddr(s); err == nil {
		return fmt.Errorf("dns suffix %q is an IP address", s)
	}
	labels := strings.Split(s, ".")
	if len(labels) < 2 {
		return fmt.Errorf("dns suffix %q: a single label is not allowed", s)
	}
	for _, l := range labels {
		if !ValidPrivateName(l) {
			return fmt.Errorf("dns suffix %q is not a valid DNS name", s)
		}
	}
	switch labels[len(labels)-1] {
	case "pd", "private", "localhost":
		return fmt.Errorf("dns suffix %q is under a sandbox-internal zone", s)
	}
	return nil
}

// UnderSuffix reports whether name is suffix or a name below it.
func UnderSuffix(name, suffix string) bool {
	return name == suffix || strings.HasSuffix(name, "."+suffix)
}

// LongestSuffix returns the longest of suffixes that name is under.
func LongestSuffix(name string, suffixes []string) (string, bool) {
	best := ""
	for _, s := range suffixes {
		if UnderSuffix(name, s) && len(s) > len(best) {
			best = s
		}
	}
	return best, best != ""
}

// CarvePlatform removes the platform ranges from ps and returns the minimal
// list that is left, sorted, with prefixes inside another folded into it.
func CarvePlatform(ps []netip.Prefix) []netip.Prefix {
	var out []netip.Prefix
	for _, p := range ps {
		left := []netip.Prefix{p.Masked()}
		for _, r := range PlatformRanges {
			var next []netip.Prefix
			for _, q := range left {
				next = append(next, subtractPrefix(q, r)...)
			}
			left = next
		}
		out = append(out, left...)
	}
	return foldPrefixes(out)
}

// subtractPrefix returns a minus b as the fewest prefixes.
func subtractPrefix(a, b netip.Prefix) []netip.Prefix {
	if !a.Overlaps(b) {
		return []netip.Prefix{a}
	}
	if b.Bits() <= a.Bits() {
		return nil
	}
	lo, hi := splitPrefix(a)
	return append(subtractPrefix(lo, b), subtractPrefix(hi, b)...)
}

// splitPrefix halves a into the two prefixes one bit longer.
func splitPrefix(a netip.Prefix) (netip.Prefix, netip.Prefix) {
	bits := a.Bits() + 1
	b := a.Addr().As4()
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	v |= 1 << (32 - bits)
	hi := netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
	return netip.PrefixFrom(a.Addr(), bits), netip.PrefixFrom(hi, bits)
}

// foldPrefixes sorts ps and drops duplicates and prefixes inside another.
func foldPrefixes(ps []netip.Prefix) []netip.Prefix {
	sort.Slice(ps, func(i, j int) bool {
		if c := ps[i].Addr().Compare(ps[j].Addr()); c != 0 {
			return c < 0
		}
		return ps[i].Bits() < ps[j].Bits()
	})
	out := []netip.Prefix{}
	for _, p := range ps {
		if n := len(out); n > 0 && out[n-1].Bits() <= p.Bits() && out[n-1].Contains(p.Addr()) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// Shadowed lists the parts of p a sandbox never routes through a connector:
// each overlapping platform range, or p itself when it lies inside one.
func Shadowed(p netip.Prefix) []string {
	out := []string{}
	for _, r := range PlatformRanges {
		switch {
		case !p.Overlaps(r):
		case r.Bits() >= p.Bits():
			out = append(out, r.String())
		default:
			out = append(out, p.String())
		}
	}
	return out
}

// ValidPrivateRoutes checks a pushed route list: canonical IPv4 subnets clear
// of the platform ranges and well-formed suffixes, within the limits.
func ValidPrivateRoutes(r PrivateRoutes) error {
	if len(r.Subnets) > MaxRoutedPrefixes || len(r.DNS) > MaxEffectiveGrants {
		return errors.New("too many private routes")
	}
	for _, s := range r.Subnets {
		p, err := netip.ParsePrefix(s)
		if err != nil || !p.Addr().Is4() || p.Masked() != p || p.Bits() == 0 {
			return fmt.Errorf("private route %q is not an IPv4 CIDR", s)
		}
		if len(Shadowed(p)) > 0 {
			return fmt.Errorf("private route %q overlaps a platform range", s)
		}
	}
	for _, d := range r.DNS {
		if err := ValidPrivateSuffix(d); err != nil {
			return err
		}
	}
	return nil
}
