package templates

import "strings"

// CustomImageSlug reduces an org or image name to what a custom image's tag can hold.
func CustomImageSlug(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		out = "img"
	}
	return out
}

// ForeignCustomImage reports whether ref, a template id or image reference, names
// another org's custom image: sf-<org>-<name>, see customImageTag. Those are built
// from a customer's context and may hold their secrets, and the image allowlist
// deliberately skips them. Docker resolves the docker.io/library spellings to the
// same local image.
//
// ponytail: matched on the tag's org prefix, which assumes no org id's slug
// extends another's; store the owner with the build if org ids ever can.
func ForeignCustomImage(org, ref string) bool {
	name := strings.ToLower(strings.TrimSpace(ref))
	for _, prefix := range []string{"index.docker.io/", "registry-1.docker.io/", "docker.io/", "library/"} {
		name = strings.TrimPrefix(name, prefix)
	}
	name = strings.TrimPrefix(name, "library/")
	return strings.HasPrefix(name, "sf-") && !strings.HasPrefix(name, "sf-"+CustomImageSlug(org)+"-")
}
