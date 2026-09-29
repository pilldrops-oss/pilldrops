//go:build linux

package vm

import "testing"

func TestOverlaySizeMB(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		requested int
		want      int
	}{
		// The literal, not the constant: a container image set does not fit in
		// the old 5 GiB, and a symbolic assertion would pass at any size.
		{"unspecified is 20 GiB", 0, 20 * 1024},
		{"negative is treated as unspecified", -1, DefaultOverlayMB},
		// mkfs.ext4 fails on an implausibly small device.
		{"below the floor is raised to it", 1, minOverlayMB},
		{"at the floor is kept", minOverlayMB, minOverlayMB},
		{"a declared cap is honoured", 1024, 1024},
		{"a large declared cap is honoured", 20480, 20480},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := overlaySizeMB(tc.requested); got != tc.want {
				t.Fatalf("overlaySizeMB(%d) = %d, want %d", tc.requested, got, tc.want)
			}
		})
	}
}
