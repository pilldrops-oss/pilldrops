package sandbox

import (
	"fmt"
	"regexp"
	"strings"
)

// slugRe is the slug grammar: a DNS label that starts with a letter, so it can
// never parse as the <port>-<id> preview host (a port starts with a digit).
var slugRe = regexp.MustCompile(`^[a-z](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// reservedSlugs are labels the platform serves itself under the sandbox domain.
var reservedSlugs = map[string]bool{
	"www": true, "api": true, "app": true, "dashboard": true, "portal": true, "mail": true,
}

// reservedForNewSlugs are refused for new assignments only. A sandbox that
// held one before it was reserved keeps it, and it keeps routing, which is
// why they are not in reservedSlugs: ValidSlug is also the routing parser.
var reservedForNewSlugs = map[string]bool{
	"login": true, "auth": true, "accounts": true, "clerk": true, "admin": true, "docs": true,
	"help": true, "support": true, "status": true, "billing": true, "share": true, "agent": true,
}

// ReservedForNewSlugs reports whether s is a platform label no sandbox may
// newly take, though one that already holds it keeps it.
func ReservedForNewSlugs(s string) bool { return reservedForNewSlugs[s] }

// AssignableSlug is ValidSlug for a slug a sandbox is about to take.
func AssignableSlug(s string) error {
	if err := ValidSlug(s); err != nil {
		return err
	}
	if reservedForNewSlugs[s] {
		return fmt.Errorf("slug %q is reserved", s)
	}
	return nil
}

// ValidLabel reports whether s is a lowercase label of at most max bytes:
// [a-z0-9] first, then lowercase letters, digits and hyphens. One grammar for
// network tags, checkpoint names and snapshot names, which differ only in max.
func ValidLabel(s string, max int) bool {
	if s == "" || len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0:
		default:
			return false
		}
	}
	return true
}

// ValidSlug reports why s cannot be a sandbox slug; "" is always valid (no slug).
func ValidSlug(s string) error {
	if s == "" {
		return nil
	}
	if len(s) > 63 {
		return fmt.Errorf("slug %q is longer than 63 characters", s)
	}
	if !slugRe.MatchString(s) {
		return fmt.Errorf("slug %q must start with a lowercase letter and continue with "+
			"lowercase letters, digits and hyphens, not ending in a hyphen", s)
	}
	if reservedSlugs[s] {
		return fmt.Errorf("slug %q is reserved", s)
	}
	if strings.HasPrefix(s, "agent-") {
		return fmt.Errorf("slug %q: the agent- prefix is reserved", s)
	}
	return nil
}
