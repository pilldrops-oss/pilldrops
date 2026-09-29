package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestConfineToWorkspace uses a real filesystem so symlink escapes are exercised.
func TestConfineToWorkspace(t *testing.T) {
	root := t.TempDir()
	old := homeDir
	homeDir = root
	t.Cleanup(func() { homeDir = old })

	if err := os.Symlink("/etc", filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		p       string
		wantErr bool
	}{
		{"foo.txt", false},                     // new leaf, relative, in-workspace
		{"sub/a/b", false},                     // new nested leaf under a real dir
		{filepath.Join(root, "ok.txt"), false}, // in-workspace absolute path
		{"/etc/shadow", true},                  // out-of-workspace absolute path
		{"../../etc/passwd", true},             // lexical traversal (relative)
		{"a/../../etc/passwd", true},           // lexical traversal, mixed
		{"escape/shadow", true},                // symlink to /etc -> rejected
		{"alias/new.txt", false},               // symlink to an in-workspace dir -> allowed
		{"", false},                            // empty path -> homeDir itself
	}
	for _, tc := range cases {
		t.Run(tc.p, func(t *testing.T) {
			_, err := confineToWorkspace(tc.p)
			if (err != nil) != tc.wantErr {
				t.Fatalf("confineToWorkspace(%q) err=%v wantErr=%v", tc.p, err, tc.wantErr)
			}
		})
	}
}

// An absolute in-workspace path must not be joined onto homeDir a second time.
func TestConfineToWorkspaceAbsoluteResolvesInPlace(t *testing.T) {
	root := t.TempDir()
	old := homeDir
	homeDir = root
	t.Cleanup(func() { homeDir = old })

	want := filepath.Join(root, "handler.py")
	got, err := confineToWorkspace(want)
	if err != nil {
		t.Fatalf("confineToWorkspace(%q) unexpected error: %v", want, err)
	}
	if got != want {
		t.Fatalf("confineToWorkspace(%q) = %q, want %q (unchanged)", want, got, want)
	}
}
