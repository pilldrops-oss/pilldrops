package docker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sourceContaining returns the one non-test source file holding marker, and
// fails if none does. One file, not a concatenation: callers slice forward from
// the marker.
func sourceContaining(t *testing.T, marker string) string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(".", n))
		if err != nil {
			t.Fatalf("read %s: %v", n, err)
		}
		if strings.Contains(string(raw), marker) {
			return string(raw)
		}
	}
	t.Fatalf("no source file in this package contains %q — it was renamed or "+
		"removed, and the tests that inspect it are no longer checking anything", marker)
	return ""
}
