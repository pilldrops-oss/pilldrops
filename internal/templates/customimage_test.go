package templates

import "testing"

func TestAnOrgCannotBootAnotherOrgsCustomImage(t *testing.T) {
	const org = "u:alice"
	for _, tc := range []struct {
		ref     string
		foreign bool
	}{
		{"sf-u-alice-tool", false},
		{"sf-u-alice-tool:latest", false},
		{"docker.io/library/sf-u-alice-tool:latest", false},
		{"", false},
		{"python-3.11", false},
		{"ghcr.io/acme/sf-tool:1", false},
		{"sf-u-bob-tool", true},
		{"sf-u-bob-tool:latest", true},
		{"SF-U-BOB-TOOL:latest", true},
		{"docker.io/library/sf-u-bob-tool:latest", true},
		{"index.docker.io/library/sf-u-bob-tool", true},
		{"library/sf-u-bob-tool", true},
		{"sf-u-alicefake-tool", true},
	} {
		if got := ForeignCustomImage(org, tc.ref); got != tc.foreign {
			t.Errorf("ForeignCustomImage(%q, %q) = %v, want %v", org, tc.ref, got, tc.foreign)
		}
	}
}
