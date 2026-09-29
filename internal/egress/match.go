package egress

import (
	"net/netip"
	"strings"
)

// Verdict is what the matcher decided and why. Reason reaches the node log
// and the tenant's sandbox event, so it names the deciding rule.
type Verdict struct {
	Allowed bool
	// Reason names the deciding rule in the operator's own vocabulary.
	Reason string
	// Entry is the rule that decided, zero when none did. The name layer reads
	// its Kind: a connection permitted by a DOMAIN entry is dialled by that
	// name, resolved in the same decision, rather than to the address the guest
	// picked — which is what a rebinding guest is counting on.
	Entry Entry
}

// Conn is one connection being judged. Name is the SNI or HTTP Host read off
// the wire, empty when absent; Port decides whether a name could be present at all.
type Conn struct {
	Name string
	Addr netip.Addr
	Port int
}

// Match decides one connection against a rule set. Allow beats deny, which is
// what makes "deny 0.0.0.0/0, allow *.github.com" expressible. A nil rule set
// is unrestricted; an empty one denies.
func Match(allow, deny []Entry, c Conn) Verdict {
	if allow == nil && deny == nil {
		return Verdict{Allowed: true, Reason: "no egress rules in force"}
	}
	if e, ok := firstMatch(allow, c); ok {
		return Verdict{Allowed: true, Reason: "allowed by " + e.Raw, Entry: e}
	}
	if e, ok := firstMatch(deny, c); ok {
		return Verdict{Allowed: false, Reason: "denied by " + e.Raw, Entry: e}
	}
	// A rule set in force denies whatever it did not name.
	return Verdict{Allowed: false, Reason: "no rule matched this destination"}
}

// firstMatch returns the first entry in list that covers c.
func firstMatch(list []Entry, c Conn) (Entry, bool) {
	for _, e := range list {
		if matches(e, c) {
			return e, true
		}
	}
	return Entry{}, false
}

func matches(e Entry, c Conn) bool {
	switch e.Kind {
	case KindAddr:
		return c.Addr.IsValid() && e.Prefix.Contains(c.Addr)
	case KindDomain:
		// A domain entry only decides a connection that carried a name, or a
		// sandbox could reach a denied host by not announcing where it is going.
		if c.Name == "" || !NameIsOnTheWire(c.Port) {
			return false
		}
		return matchDomain(e, c.Name)
	}
	return false
}

// matchDomain compares one domain entry against a name from the wire. A
// wildcard covers subdomains but not the bare parent. The suffix is anchored
// on the dot so "github.com" does not match "evilgithub.com".
func matchDomain(e Entry, name string) bool {
	n := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	if n == "" {
		return false
	}
	if !e.Wildcard {
		return n == e.Domain
	}
	suffix := "." + e.Domain
	return len(n) > len(suffix) && strings.HasSuffix(n, suffix)
}
