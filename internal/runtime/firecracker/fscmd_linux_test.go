//go:build linux

package firecracker

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// runIn runs a guest helper command as envd does, from the sandbox home, and
// classifies its failure as fcFS.run does.
func runIn(t *testing.T, root, p, cmd string) error {
	t.Helper()
	c := exec.Command("/bin/sh", "-c", cmd)
	c.Dir = root
	out, err := c.CombinedOutput()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fsError(p, string(out), ee.ExitCode())
	}
	return err
}

// Listing a file and removing the home answer as on docker: 400 and 400,
// where a list of a file was a 200 with no entries.
func TestListOfAFileAndRemoveOfTheHomeAreRefused(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "afile"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runIn(t, root, "afile", listCmd(root, "afile")); !errors.Is(err, fs.ErrInvalid) {
		t.Errorf("list of a file = %v, want fs.ErrInvalid (400)", err)
	}
	if err := runIn(t, root, "missing", listCmd(root, "missing")); !errors.Is(err, rt.ErrPathNotFound) {
		t.Errorf("list of a missing path = %v, want ErrPathNotFound (404)", err)
	}
	if err := runIn(t, root, ".", listCmd(root, ".")); err != nil {
		t.Errorf("list of the home = %v", err)
	}
	for _, p := range []string{".", root} {
		if err := runIn(t, root, p, removeCmd(root, p)); !errors.Is(err, fs.ErrInvalid) {
			t.Errorf("remove %q = %v, want fs.ErrInvalid (400)", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "afile")); err != nil {
		t.Errorf("a refused remove of the home still deleted its contents: %v", err)
	}
	if err := runIn(t, root, "afile", removeCmd(root, "afile")); err != nil {
		t.Errorf("remove of a file = %v", err)
	}
}
