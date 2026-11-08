package process

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

func TestFSRoundTrip(t *testing.T) {
	root := t.TempDir()
	fs := &workdirFS{root: root}

	if err := fs.Write(t.Context(), "a/b.txt", []byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := fs.Read(t.Context(), "a/b.txt")
	if err != nil || string(got) != "hello" {
		t.Fatalf("read = %q, %v", got, err)
	}
	if err := fs.Mkdir(t.Context(), "d"); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	entries, err := fs.List(t.Context(), "a")
	if err != nil || len(entries) != 1 || entries[0].Name != "b.txt" {
		t.Fatalf("list = %+v, %v", entries, err)
	}
	info, err := fs.Stat(t.Context(), "a/b.txt")
	if err != nil || info.Size != 5 {
		t.Fatalf("stat = %+v, %v", info, err)
	}
	if err := fs.Move(t.Context(), "a/b.txt", "a/c.txt"); err != nil {
		t.Fatalf("move: %v", err)
	}
	if err := fs.Remove(t.Context(), "a/c.txt"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := fs.Read(t.Context(), "a/c.txt"); err == nil {
		t.Fatalf("expected read of removed file to fail")
	}
}

func TestFSRejectsTraversal(t *testing.T) {
	fs := &workdirFS{root: t.TempDir()}
	if err := fs.Write(t.Context(), "../escape.txt", []byte("x")); err == nil {
		t.Fatalf("expected traversal write to be rejected")
	}
	if _, err := fs.Read(t.Context(), "../../etc/passwd"); err == nil {
		t.Fatalf("expected traversal read to be rejected")
	}
	_ = filepath.Separator
}

func TestFSTakesAnAbsolutePathInsideRoot(t *testing.T) {
	root := t.TempDir()
	fs := &workdirFS{root: root}
	if err := fs.Write(t.Context(), filepath.Join(root, "abs.txt"), []byte("x")); err != nil {
		t.Fatalf("write by absolute path: %v", err)
	}
	if got, err := fs.Read(t.Context(), "abs.txt"); err != nil || string(got) != "x" {
		t.Fatalf("read back = %q, %v, want the file written at root", got, err)
	}
	if _, err := fs.Read(t.Context(), "/etc/passwd"); err == nil {
		t.Fatal("an absolute path outside root was read")
	}
}

func TestFSErrorsHideHostRoot(t *testing.T) {
	root := t.TempDir()
	fs := &workdirFS{root: root}

	_, err := fs.Read(t.Context(), "app/missing.txt")
	if err == nil {
		t.Fatal("expected read of missing file to fail")
	}
	if strings.Contains(err.Error(), root) {
		t.Fatalf("error leaks host root %q: %v", root, err)
	}
	if !strings.Contains(err.Error(), "/app/missing.txt") {
		t.Fatalf("error should name the sandbox-relative path: %v", err)
	}

	if _, err := fs.Stat(t.Context(), "nope"); err == nil || strings.Contains(err.Error(), root) {
		t.Fatalf("stat error should fail without leaking root: %v", err)
	}
}

func TestBootExecAndPause(t *testing.T) {
	r := New(t.TempDir())
	ctx := context.Background()
	inst, err := r.Boot(ctx, rt.BootSpec{SandboxID: "sb1", VCPU: 1, MemMiB: 512})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	res, err := inst.Exec(ctx, rt.ExecSpec{Cmd: "echo hello"})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.ExitCode != 0 || res.Stdout != "hello\n" {
		t.Fatalf("exec result = %+v", res)
	}
	res, err = inst.Exec(ctx, rt.ExecSpec{Cmd: "exit 3"})
	if err != nil {
		t.Fatalf("exec exit3: %v", err)
	}
	if res.ExitCode != 3 {
		t.Fatalf("exit code = %d, want 3", res.ExitCode)
	}

	pi := inst.(*processInstance)
	pi.Pause()
	if _, err := inst.Exec(ctx, rt.ExecSpec{Cmd: "echo nope"}); err != rt.ErrPaused {
		t.Fatalf("exec while paused = %v, want ErrPaused", err)
	}
	pi.Resume()
	if _, err := inst.Exec(ctx, rt.ExecSpec{Cmd: "echo ok"}); err != nil {
		t.Fatalf("exec after resume: %v", err)
	}
}

func TestStartServerCapturesAppLog(t *testing.T) {
	r := New(t.TempDir())
	ctx := context.Background()
	inst, err := r.Boot(ctx, rt.BootSpec{SandboxID: "sb1", VCPU: 1, MemMiB: 512})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	if err := inst.StartServer(ctx, "echo out-line; echo err-line >&2; sleep 30", nil); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer inst.Stop(ctx)

	var log string
	for i := 0; i < 50; i++ {
		if b, err := inst.FS().Read(t.Context(), ".sandboxd/app.log"); err == nil {
			log = string(b)
			if strings.Contains(log, "out-line") && strings.Contains(log, "err-line") {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(log, "out-line") || !strings.Contains(log, "err-line") {
		t.Fatalf("app log missing output; got %q", log)
	}
}

func TestFSRemoveRefusesRoot(t *testing.T) {
	root := t.TempDir()
	fs := &workdirFS{root: root}
	if err := fs.Write(t.Context(), "keep.txt", []byte("important")); err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, p := range []string{"", ".", "/", "./"} {
		if err := fs.Remove(t.Context(), p); err == nil {
			t.Fatalf("Remove(%q) should be rejected, got nil", p)
		}
	}
	if _, err := fs.Read(t.Context(), "keep.txt"); err != nil {
		t.Fatalf("root file was deleted by a root-resolving Remove: %v", err)
	}
}

func TestSnapshotRestore(t *testing.T) {
	dir := t.TempDir()
	r := New(dir)
	ctx := context.Background()
	inst, _ := r.Boot(ctx, rt.BootSpec{SandboxID: "sb2"})
	if err := inst.FS().Write(t.Context(), "state.txt", []byte("persisted")); err != nil {
		t.Fatalf("write: %v", err)
	}
	snap := rt.SnapshotRef{Path: dir + "/sb2.tar"}
	if err := inst.Snapshot(ctx, snap); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if err := inst.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	restored, err := r.Restore(ctx, snap, rt.BootSpec{SandboxID: "sb2"})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, err := restored.FS().Read(t.Context(), "state.txt")
	if err != nil || string(got) != "persisted" {
		t.Fatalf("restored read = %q, %v", got, err)
	}
}

func TestStartServerInjectsEnv(t *testing.T) {
	r := New(t.TempDir())
	ctx := context.Background()
	inst, err := r.Boot(ctx, rt.BootSpec{SandboxID: "sb1", VCPU: 1, MemMiB: 512})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	if err := inst.StartServer(ctx, `echo "GREETING=$GREETING"; sleep 30`,
		map[string]string{"GREETING": "hello-env"}); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer inst.Stop(ctx)

	var log string
	for i := 0; i < 50; i++ {
		if b, err := inst.FS().Read(t.Context(), ".sandboxd/app.log"); err == nil {
			log = string(b)
			if strings.Contains(log, "hello-env") {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(log, "GREETING=hello-env") {
		t.Fatalf("env not injected; log=%q", log)
	}
}
