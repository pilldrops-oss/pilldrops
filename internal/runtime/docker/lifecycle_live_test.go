package docker

import (
	"strconv"
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

// A pid from Procs must be one Signal can reach, and the killed process must
// be reaped rather than left a zombie that still shows as running.
func TestLiveSignalKillsAListedProcess(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Skipf("docker runtime unavailable: %v", err)
	}
	ctx := t.Context()
	inst, err := r.Boot(ctx, rt.BootSpec{
		SandboxID: "sb-live-sig-" + time.Now().Format("150405"),
		Template:  rt.Template{Name: "base", Image: "alpine:3"},
		VCPU:      1, MemMiB: 256,
	})
	if err != nil {
		t.Skipf("boot failed (image or daemon unavailable): %v", err)
	}
	t.Cleanup(func() { _ = inst.Stop(t.Context()) })

	if _, err := inst.Exec(ctx, rt.ExecSpec{Cmd: "sleep 777", Detach: true}); err != nil {
		t.Fatalf("exec: %v", err)
	}
	procs, err := inst.Procs()
	if err != nil {
		t.Fatalf("procs: %v", err)
	}
	pid := 0
	for _, p := range procs {
		if strings.HasPrefix(p.Cmd, "sleep") && p.PID > pid {
			pid = p.PID
		}
	}
	if pid == 0 {
		t.Fatalf("procs does not list the sleep: %+v", procs)
	}
	if err := inst.Signal(pid, 9); err != nil {
		t.Fatalf("signal %d: %v", pid, err)
	}
	time.Sleep(500 * time.Millisecond)
	res, err := inst.Exec(ctx, rt.ExecSpec{Cmd: "cat /proc/" + strconv.Itoa(pid) + "/stat 2>/dev/null || echo gone"})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "gone" {
		t.Errorf("pid %d is still there after SIGKILL: %s", pid, res.Stdout)
	}
}

// A container that dies underneath its sandbox must read as dead, or the
// sandbox lists as running until its TTL.
func TestLiveAliveSeesAContainerThatDied(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Skipf("docker runtime unavailable: %v", err)
	}
	ctx := t.Context()
	inst, err := r.Boot(ctx, rt.BootSpec{
		SandboxID: "sb-live-dead-" + time.Now().Format("150405"),
		Template:  rt.Template{Name: "base", Image: "alpine:3"},
		VCPU:      1, MemMiB: 256,
	})
	if err != nil {
		t.Skipf("boot failed (image or daemon unavailable): %v", err)
	}
	t.Cleanup(func() { _ = inst.Stop(t.Context()) })
	lv := inst.(rt.Liveness)
	if !lv.Alive() {
		t.Fatal("a running container reads as dead")
	}
	if _, _, code, err := dockerOut(ctx, "kill", inst.(*dockerInstance).cid); err != nil || code != 0 {
		t.Fatalf("docker kill: %v %d", err, code)
	}
	old := deadConfirmAfter
	deadConfirmAfter = 0
	t.Cleanup(func() { deadConfirmAfter = old })
	if !lv.Alive() {
		t.Error("one stopped read already reads as dead")
	}
	if lv.Alive() {
		t.Error("a killed container reads as alive on the second read")
	}
	if inst.(rt.OOMKilledReporter).OOMKilled() {
		t.Error("a plain kill reads as an OOM kill")
	}
}

// A service image may have no /bin/sh to list processes with.
func TestLiveProcsWithoutAShell(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Skipf("docker runtime unavailable: %v", err)
	}
	ctx := t.Context()
	inst, err := r.Boot(ctx, rt.BootSpec{
		SandboxID: "sb-live-nosh-" + time.Now().Format("150405"),
		Template:  rt.Template{Name: "base", Image: "alpine:3"},
		VCPU:      1, MemMiB: 256,
	})
	if err != nil {
		t.Skipf("boot failed (image or daemon unavailable): %v", err)
	}
	t.Cleanup(func() { _ = inst.Stop(t.Context()) })
	cid := inst.(*dockerInstance).cid
	if _, se, code, err := dockerOut(ctx, "exec", "-u", "0", cid, "rm", "/bin/sh"); err != nil || code != 0 {
		t.Fatalf("remove /bin/sh: %v %d %s", err, code, se)
	}
	procs, err := inst.Procs()
	if err != nil || len(procs) == 0 {
		t.Fatalf("procs without a shell = %v, %v; want the container's processes", procs, err)
	}
	for _, p := range procs {
		if p.PID != 0 {
			t.Errorf("%q listed with host pid %d, which Signal would send into the container", p.Cmd, p.PID)
		}
	}
}
