package httpapi

import (
	"fmt"
	"regexp"
	"strings"
)

var digestRefRe = regexp.MustCompile(`@sha256:[a-f0-9]{64}$`)

// imageAllowed enforces the registry allowlist and digest-pin policy for a
// tenant-supplied image. An empty allowlist allows any registry.
func imageAllowed(img string, allowlist []string, requireDigest bool) error {
	if requireDigest && !digestRefRe.MatchString(img) {
		return fmt.Errorf("image must be pinned to a digest (name@sha256:...)")
	}
	if len(allowlist) == 0 {
		return nil
	}
	for _, prefix := range allowlist {
		if prefixMatchesRegistry(img, prefix) {
			return nil
		}
	}
	return fmt.Errorf("image registry not in allowlist")
}

// prefixMatchesRegistry requires a component boundary after a bare-host entry
// so "ghcr.io" does not match "ghcr.io.evil.com".
func prefixMatchesRegistry(img, prefix string) bool {
	if !strings.HasPrefix(img, prefix) {
		return false
	}
	if strings.HasSuffix(prefix, "/") {
		return true
	}
	rest := img[len(prefix):]
	return rest == "" || rest[0] == '/' || rest[0] == ':' || rest[0] == '@'
}
