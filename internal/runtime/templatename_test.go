package runtime

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// The failure message shows what filepath.Join would have resolved the name to.
func TestValidateTemplateNameRejectsEverythingThatMovesAPath(t *testing.T) {
	const snapDir = "/var/lib/sandboxd/snapshots"
	bad := []string{
		"..",
		"../../../etc",
		"../..",
		"a/../..",
		"sub/dir",
		"/etc",
		"/etc/passwd",
		`..\..\windows`,
		".hidden",
		"-flag",
		"_leading",
		".",
		"./x",
		"",
		"UPPER",
		"has space",
		"nul\x00byte",
		"tilde~",
		"semi;colon",
		"dollar$var",
		"unicode-é",
		strings.Repeat("a", MaxTemplateNameLen+1),
	}
	for _, name := range bad {
		err := ValidateTemplateName(name)
		if err == nil {
			joined := filepath.Join(snapDir, name, "rootfs.squashfs")
			t.Errorf("ValidateTemplateName(%q) = nil; that name joins to %q", name, joined)
			continue
		}
		if !errors.Is(err, ErrInvalidTemplateName) {
			t.Errorf("ValidateTemplateName(%q) returned %v, which does not wrap ErrInvalidTemplateName "+
				"— handleCreate maps on that sentinel and would answer 503 instead of 400", name, err)
		}
	}
}

// Asserts on the join result itself, not on the validator's rule.
func TestValidatedNamesCannotLeaveTheSnapshotDir(t *testing.T) {
	const snapDir = "/var/lib/sandboxd/snapshots"
	for _, name := range []string{
		"base", "code-interpreter", "python-3.11", "android-headless",
		"a", "0", "sf_custom.image-1",
	} {
		if err := ValidateTemplateName(name); err != nil {
			t.Fatalf("ValidateTemplateName(%q) = %v, but this is a shape the product uses", name, err)
		}
		joined := filepath.Join(snapDir, name, "rootfs.squashfs")
		want := snapDir + "/" + name + "/rootfs.squashfs"
		if joined != want {
			t.Errorf("Join(%q) = %q, want %q — an accepted name changed the path shape", name, joined, want)
		}
		if !strings.HasPrefix(joined, snapDir+"/") {
			t.Errorf("accepted name %q escapes %s: %q", name, snapDir, joined)
		}
	}
}

func TestValidatorIsNotVacuous(t *testing.T) {
	if ValidateTemplateName("../../../etc") == nil {
		t.Fatal("ValidateTemplateName accepts \"../../../etc\": it is not validating anything")
	}
	if ValidateTemplateName("base") != nil {
		t.Fatal("ValidateTemplateName rejects \"base\": it refuses everything, which is equally useless")
	}
}
