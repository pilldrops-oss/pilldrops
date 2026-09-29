package process

import (
	"archive/tar"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTar writes an archive of name to content pairs and returns its path.
func writeTar(t *testing.T, entries map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "snap.tar")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	for name, content := range entries {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(content)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// The caps are lowered rather than shipping a multi-GiB fixture; the control
// proves a red result is the cap firing, not the harness.
func TestUntarRejectsABombAndPassesTheControl(t *testing.T) {
	oldBytes, oldEntries := maxUntarBytes, maxUntarEntries
	maxUntarBytes, maxUntarEntries = 64, 4
	t.Cleanup(func() { maxUntarBytes, maxUntarEntries = oldBytes, oldEntries })

	// Two entries, each under the cap alone.
	over := writeTar(t, map[string]string{
		"a": strings.Repeat("x", 40),
		"b": strings.Repeat("x", 40),
	})
	if err := untar(over, t.TempDir()); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("an 80-byte archive passed a 64-byte cap: err=%v", err)
	}

	many := map[string]string{}
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		many[n] = "x"
	}
	if err := untar(writeTar(t, many), t.TempDir()); err == nil || !strings.Contains(err.Error(), "entries") {
		t.Fatalf("5 entries passed a 4-entry cap: err=%v", err)
	}

	// Control: an archive under both limits extracts intact.
	okDir := t.TempDir()
	if err := untar(writeTar(t, map[string]string{"ok": "payload"}), okDir); err != nil {
		t.Fatalf("control failed: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(okDir, "ok"))
	if err != nil || string(got) != "payload" {
		t.Fatalf("control content wrong: %q, %v", got, err)
	}
}
