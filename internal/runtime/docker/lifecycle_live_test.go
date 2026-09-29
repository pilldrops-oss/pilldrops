package docker

import (
	"strings"
	"testing"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

func TestLiveRuntimeLifecycle(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Skipf("docker runtime unavailable: %v", err)
	}
	ctx := t.Context()

	inst, err := r.Boot(ctx, rt.BootSpec{
		SandboxID: "sb-live-" + time.Now().Format("150405"),
		Template:  rt.Template{Name: "base", Image: "alpine:3"},
		VCPU:      1, MemMiB: 256,
	})
	if err != nil {
		t.Skipf("boot failed (image or daemon unavailable): %v", err)
	}
	t.Cleanup(func() { _ = inst.Stop(t.Context()) })

	res, err := inst.Exec(ctx, rt.ExecSpec{Cmd: "echo out; echo err 1>&2; exit 3"})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !strings.Contains(res.Stdout, "out") {
		t.Errorf("stdout = %q", res.Stdout)
	}
	if !strings.Contains(res.Stderr, "err") {
		t.Errorf("stderr = %q", res.Stderr)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit = %d, want 3", res.ExitCode)
	}

	res, err = inst.Exec(ctx, rt.ExecSpec{Cmd: "echo $CUSTOM", Env: map[string]string{"CUSTOM": "injected"}})
	if err != nil || !strings.Contains(res.Stdout, "injected") {
		t.Errorf("env injection: err=%v stdout=%q", err, res.Stdout)
	}

	fsys := inst.FS()
	if err := fsys.Write(ctx, "hello.txt", []byte("filesystem works")); err != nil {
		t.Fatalf("fs write: %v", err)
	}
	got, err := fsys.Read(ctx, "hello.txt")
	if err != nil || string(got) != "filesystem works" {
		t.Fatalf("fs read: err=%v got=%q", err, got)
	}
	entries, err := fsys.List(ctx, ".")
	if err != nil {
		t.Fatalf("fs list: %v", err)
	}
	var seen bool
	for _, e := range entries {
		if e.Name == "hello.txt" {
			seen = true
		}
	}
	if !seen {
		t.Errorf("list did not include the file just written: %+v", entries)
	}

	procs, err := inst.Procs()
	if err != nil {
		t.Fatalf("procs: %v", err)
	}
	if len(procs) == 0 {
		t.Error("procs returned nothing for a running container")
	}

	if du, ok := inst.(rt.DiskUsager); ok {
		if _, err := du.DiskUsageMB(ctx); err != nil {
			t.Errorf("disk usage: %v", err)
		}
	}
}
