package templates

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPublishedIDs(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"base", "code-interpreter"} {
		if err := os.MkdirAll(filepath.Join(dir, id), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, id, "rootfs.squashfs"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A half-built template with no rootfs must not be advertised.
	if err := os.MkdirAll(filepath.Join(dir, "broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := Dir()
	SetDir(dir)
	defer SetDir(old)

	got := PublishedIDs()
	want := []string{"base", "code-interpreter"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("PublishedIDs() = %v, want %v", got, want)
	}
}
