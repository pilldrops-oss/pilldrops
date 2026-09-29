package docker

import (
	"errors"
	"io/fs"
	"os/exec"
	"strings"
	"testing"
)

// liveFS starts a throwaway container and returns a driver over it, or skips.
func liveFS(t *testing.T) *dockerFS {
	t.Helper()
	out, err := exec.Command("docker", "run", "-d", "--rm", "alpine:3", "sleep", "120").Output()
	if err != nil {
		t.Skipf("cannot start a container: %v", err)
	}
	cid := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", cid).Run() })
	return &dockerFS{cid: cid, eng: newEngine()}
}

func TestLiveFSRoundTrip(t *testing.T) {
	d := liveFS(t)
	ctx := t.Context()

	const body = "hello from the filesystem driver\n"
	if err := d.Write(ctx, "/tmp/deep/nested/file.txt", []byte(body)); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := d.Read(ctx, "/tmp/deep/nested/file.txt")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != body {
		t.Errorf("read back %q, want %q", got, body)
	}

	st, err := d.Stat(ctx, "/tmp/deep/nested/file.txt")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Size != int64(len(body)) || st.IsDir {
		t.Errorf("stat = %+v, want a %d-byte file", st, len(body))
	}

	entries, err := d.List(ctx, "/tmp/deep/nested")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found bool
	for _, e := range entries {
		if e.Name == "file.txt" && e.Size == int64(len(body)) {
			found = true
		}
	}
	if !found {
		t.Errorf("list did not report file.txt with the right size: %+v", entries)
	}

	if err := d.Move(ctx, "/tmp/deep/nested/file.txt", "/tmp/moved.txt"); err != nil {
		t.Fatalf("move: %v", err)
	}
	if _, err := d.Read(ctx, "/tmp/moved.txt"); err != nil {
		t.Errorf("read after move: %v", err)
	}
	if err := d.Remove(ctx, "/tmp/moved.txt"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := d.Read(ctx, "/tmp/moved.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("read after remove: err = %v, want fs.ErrNotExist", err)
	}
}

func TestLiveFSMissingFileIsNotExist(t *testing.T) {
	d := liveFS(t)
	if _, err := d.Read(t.Context(), "/tmp/definitely-absent"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want fs.ErrNotExist", err)
	}
	if _, err := d.Stat(t.Context(), "/tmp/definitely-absent"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat err = %v, want fs.ErrNotExist", err)
	}
}

func TestLiveFSNoCommandInjectionThroughEngine(t *testing.T) {
	d := liveFS(t)
	ctx := t.Context()
	evil := `/tmp/x$(touch /tmp/PWNED)y`

	_ = d.Mkdir(ctx, evil)
	_ = d.Write(ctx, evil, []byte("data"))
	_, _ = d.Stat(ctx, evil)
	_, _ = d.List(ctx, "/tmp")
	_ = d.Move(ctx, evil, evil+"2")
	_ = d.Remove(ctx, evil+"2")

	if _, _, code, _ := d.exec(ctx, "test", "-e", "/tmp/PWNED"); code == 0 {
		t.Fatal("COMMAND INJECTION: /tmp/PWNED exists — the path was executed as shell")
	}
}

func TestLiveFSAwkwardFilenames(t *testing.T) {
	d := liveFS(t)
	ctx := t.Context()
	for _, name := range []string{
		"/tmp/with space.txt",
		`/tmp/with'quote.txt`,
		`/tmp/with"dquote.txt`,
		"/tmp/with$dollar.txt",
		"/tmp/with`backtick.txt",
	} {
		if err := d.Write(ctx, name, []byte(name)); err != nil {
			t.Errorf("write %q: %v", name, err)
			continue
		}
		got, err := d.Read(ctx, name)
		if err != nil {
			t.Errorf("read %q: %v", name, err)
			continue
		}
		if string(got) != name {
			t.Errorf("round-trip %q gave %q", name, got)
		}
	}
}
