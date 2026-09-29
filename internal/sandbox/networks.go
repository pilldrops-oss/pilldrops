package sandbox

import (
	"fmt"
)

// MaxNetworkTags caps a sandbox's membership. Each tag costs a set of
// host rules quadratic in its own membership, and eight is already more
// overlapping networks than a topology anyone can reason about.
const MaxNetworkTags = 8

// MaxNetworkTagLen is the tag grammar's length. Deliberately narrower than a
// DNS label — a tag names a kernel object and appears in operator output, so
// no leading hyphen, no uppercase, no unicode (ValidLabel).
const MaxNetworkTagLen = 32

// ValidNetworks reports why tags cannot be a sandbox's network membership.
// Empty (or nil) is always valid and means the sandbox joins no network, which
// is the default and reaches no peer.
func ValidNetworks(tags []string) error {
	if len(tags) > MaxNetworkTags {
		return fmt.Errorf("a sandbox may name at most %d network tags, got %d", MaxNetworkTags, len(tags))
	}
	seen := make(map[string]bool, len(tags))
	for _, t := range tags {
		if !ValidLabel(t, MaxNetworkTagLen) {
			return fmt.Errorf("network tag %q must be 1-%d characters of lowercase letters, "+
				"digits and hyphens, starting with a letter or digit", t, MaxNetworkTagLen)
		}
		if seen[t] {
			return fmt.Errorf("network tag %q is named twice", t)
		}
		seen[t] = true
	}
	return nil
}
