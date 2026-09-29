package nodeboot

import (
	"os"
	"path/filepath"
	"testing"
)

// An install from before DataDir existed has a 0755 data dir; boot narrows it.
func TestDataDirNarrowsAnExistingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := DataDir(dir); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o700 {
		t.Fatalf("data dir mode = %o, want 700", got)
	}
}
