package docker

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

func TestDockerFSAbs(t *testing.T) {
	d := &dockerFS{cid: "c"}
	cases := []struct{ in, want string }{
		{"/tmp/shots/01.png", "/tmp/shots/01.png"},
		{"/home/androidusr/summary.txt", "/home/androidusr/summary.txt"},
		{"/test/artifacts/video.mp4", "/test/artifacts/video.mp4"},
		{"/tmp/../tmp/x", "/tmp/x"},
		{"app.py", "/workspace/app.py"},
		{"sub/dir/f", "/workspace/sub/dir/f"},
		{"../../etc/passwd", "/workspace/etc/passwd"},
		{"./x", "/workspace/x"},
	}
	for _, c := range cases {
		if got := d.abs(c.in); got != c.want {
			t.Errorf("abs(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Listing a file and removing the home answer as on firecracker: 400 and 400,
// where a list of a file was a 404 and a remove of the home emptied it and
// then answered 403.
func TestListOfAFileAndRemoveOfTheHomeAreRefused(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "afile")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	list := func(p string) error {
		var stderr []byte
		cmd := exec.Command("/bin/sh", "-c", listScript, "sh", p)
		_, err := cmd.Output()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			stderr = ee.Stderr
		}
		return fsFail("list "+p, err, 0, string(stderr))
	}
	if err := list(file); !errors.Is(err, fs.ErrInvalid) {
		t.Errorf("list of a file = %v, want fs.ErrInvalid (400)", err)
	}
	if err := list(filepath.Join(dir, "missing")); !errors.Is(err, rt.ErrPathNotFound) {
		t.Errorf("list of a missing path = %v, want ErrPathNotFound (404)", err)
	}
	d := &dockerFS{cid: "c"}
	for _, p := range []string{".", "", "/workspace", "/workspace/"} {
		if err := d.Remove(context.Background(), p); !errors.Is(err, fs.ErrInvalid) {
			t.Errorf("remove %q = %v, want fs.ErrInvalid before anything runs", p, err)
		}
	}
}
