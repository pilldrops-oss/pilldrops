package templates

import (
	"os"
	"path/filepath"
	"testing"
)

// The spec's own scenario: "a sandbox runs a caller-supplied image that does
// not hold the desktop binaries -> has_desktop SHALL be false". This failed
// live and no unit test could see it, because the discriminator was Resolve,
// which also answers for anything PUBLISHED ON THE NODE — every directory
// holding a rootfs.squashfs is a resolvable template, manifest or not. A
// customer's own published image therefore claimed a desktop it has no
// binaries for. Measured on a Firecracker node with a `base-nodesktop` image
// built using the PD_NO_DESKTOP=1 opt-out: has_desktop came back true.
func TestHasDesktopStackIgnoresLocallyPublishedImages(t *testing.T) {
	dir := t.TempDir()
	// Two published images, exactly as scripts/build-template.sh leaves them:
	// a directory with a rootfs.squashfs and no manifest.
	for _, id := range []string{"base-nodesktop", "acme-private-build"} {
		if err := os.MkdirAll(filepath.Join(dir, id), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, id, "rootfs.squashfs"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	SetDir(dir)
	t.Cleanup(func() { SetDir("") })

	// Both resolve — that is the trap, and the reason this has to be asserted.
	for _, id := range []string{"base-nodesktop", "acme-private-build"} {
		if _, ok := Resolve(id); !ok {
			t.Fatalf("%s does not resolve, so this test is not exercising the trap", id)
		}
		if HasDesktopStack(id) {
			t.Errorf("HasDesktopStack(%q) = true; a locally published image the "+
				"platform did not bake the screen stack into must read false", id)
		}
	}

	// And a real catalogue template still reads true while that directory is set.
	if !HasDesktopStack("base") {
		t.Error("HasDesktopStack(\"base\") = false; the platform bakes the layer into every catalogue template")
	}
}

func TestHasDesktopStackByTemplateKind(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want bool
		why  string
	}{
		{"base", true, "a catalogue template the platform bakes"},
		{"python-3.11", true, "an alias of one"},
		{"desktop", true, "the desktop template itself"},
		{"postgres", false, "a service template runs a vendor image with its own entrypoint"},
		{"redis", false, "same"},
		{"ghcr.io/someone/thing:1", false, "a caller-supplied image"},
		{"", false, "no template at all"},
	} {
		if got := HasDesktopStack(tc.id); got != tc.want {
			t.Errorf("HasDesktopStack(%q) = %v, want %v — %s", tc.id, got, tc.want, tc.why)
		}
	}
}
