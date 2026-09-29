package httpapi

import (
	"strings"
	"testing"
)

func TestImageAllowed(t *testing.T) {
	cases := []struct {
		name          string
		img           string
		allowlist     []string
		requireDigest bool
		wantErr       bool
	}{
		{"empty allowlist allows anything", "docker.io/library/postgres:16", nil, false, false},
		{"prefix match allowed", "ghcr.io/acme/app:1", []string{"ghcr.io/", "docker.io/"}, false, false},
		{"no prefix match rejected", "evil.example.com/x:1", []string{"ghcr.io/"}, false, true},
		{"requireDigest rejects tag-only", "postgres:16", nil, true, true},
		{"requireDigest accepts digest", "postgres@sha256:" + repeat64("a"), nil, true, false},
		{"digest + allowlist both enforced", "ghcr.io/x@sha256:" + repeat64("b"), []string{"ghcr.io/"}, true, false},
		{"bare-host entry rejects look-alike registry", "ghcr.io.evil.com/x:1", []string{"ghcr.io"}, false, true},
		{"bare-host entry allows its real registry", "ghcr.io/acme/app:16", []string{"ghcr.io"}, false, false},
		{"repo-level entry rejects repo look-alike", "ghcr.io/acme-evil/x:1", []string{"ghcr.io/acme"}, false, true},
		{"repo-level entry allows its real repo", "ghcr.io/acme/app:16", []string{"ghcr.io/acme"}, false, false},
		{"trailing-slash entry allows plain prefix", "docker.io/library/postgres:16", []string{"docker.io/"}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := imageAllowed(tc.img, tc.allowlist, tc.requireDigest)
			if (err != nil) != tc.wantErr {
				t.Fatalf("imageAllowed(%q)=%v wantErr=%v", tc.img, err, tc.wantErr)
			}
		})
	}
}

func repeat64(s string) string { return strings.Repeat(s, 64) }
