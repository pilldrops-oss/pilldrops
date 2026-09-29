//go:build linux

package vm

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// A diff is sparse where pages are clean; applyDiff must write exactly the
// data extents onto the base and leave every clean byte alone.
func TestApplyDiffWritesOnlyDirtyExtents(t *testing.T) {
	dir := t.TempDir()
	const page, pages = 4096, 8
	base := bytes.Repeat([]byte{'b'}, page*pages)
	basePath := filepath.Join(dir, "mem")
	if err := os.WriteFile(basePath, base, 0o600); err != nil {
		t.Fatal(err)
	}

	// Dirty pages 1 and 5-6; everything else is a hole.
	diffPath := filepath.Join(dir, "diff")
	f, err := os.Create(diffPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(page * pages); err != nil {
		t.Fatal(err)
	}
	for _, p := range []int{1, 5, 6} {
		if _, err := f.WriteAt(bytes.Repeat([]byte{'d'}, page), int64(p*page)); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()

	n, err := applyDiff(diffPath, basePath)
	if err != nil {
		t.Fatalf("applyDiff: %v", err)
	}
	if n != 3*page {
		t.Fatalf("copied %d bytes, want %d (three dirty pages)", n, 3*page)
	}
	got, _ := os.ReadFile(basePath)
	for p := 0; p < pages; p++ {
		want := byte('b')
		if p == 1 || p == 5 || p == 6 {
			want = 'd'
		}
		if got[p*page] != want || got[p*page+page-1] != want {
			t.Fatalf("page %d = %q, want %q", p, got[p*page], want)
		}
	}
	// Mutation check: a diff with no data must change nothing and copy nothing.
	empty := filepath.Join(dir, "empty")
	if err := os.Truncate(empty, page*pages); !os.IsNotExist(err) && err != nil {
		t.Fatal(err)
	}
	ef, _ := os.Create(empty)
	ef.Truncate(page * pages)
	ef.Close()
	if n, err := applyDiff(empty, basePath); err != nil || n != 0 {
		t.Fatalf("empty diff copied %d bytes, err %v", n, err)
	}
}
