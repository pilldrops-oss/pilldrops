package artifact_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
)

func TestFSConformance(t *testing.T) {
	t.Parallel()
	runConformance(t, func(t *testing.T) artifact.StorageProvider {
		t.Helper()
		s, err := artifact.NewFS(t.TempDir())
		if err != nil {
			t.Fatalf("NewFS: %v", err)
		}
		return s
	})
}

func TestNewFSRejectsAnEmptyRoot(t *testing.T) {
	t.Parallel()
	if _, err := artifact.NewFS(""); err == nil {
		t.Fatal("want an error for an empty root")
	}
}

// ValidateKey cannot see a symlink inside the root; the post-resolution check can.
func TestFSSymlinkCannotEscapeTheRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	s, err := artifact.NewFS(root)
	if err != nil {
		t.Fatal(err)
	}
	// A legal key that resolves outside the store through the symlink.
	err = s.Put(t.Context(), "escape/pwned", strings.NewReader("owned"), 5)
	if _, statErr := os.Stat(filepath.Join(outside, "pwned")); statErr == nil {
		t.Fatalf("a symlinked key wrote outside the store root (put err: %v)", err)
	}
}
