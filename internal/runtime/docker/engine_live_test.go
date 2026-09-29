package docker

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// liveContainer starts a throwaway container, or skips when Docker is absent.
func liveContainer(t *testing.T) (*engine, string) {
	t.Helper()
	e := newEngine()
	if e == nil {
		t.Skip("no reachable docker socket")
	}
	out, err := exec.Command("docker", "run", "-d", "--rm", "alpine:3", "sleep", "120").Output()
	if err != nil {
		t.Skipf("cannot start a container: %v", err)
	}
	cid := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", cid).Run() })
	return e, cid
}

// The daemon multiplexes stdout and stderr into one framed stream; they must come back separated.
func TestEngineExecSeparatesStreamsAndReportsExit(t *testing.T) {
	e, cid := liveContainer(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	so, se, code, err := e.Exec(ctx, cid, execConfig{
		Cmd: []string{"/bin/sh", "-c", "echo to-stdout; echo to-stderr 1>&2; exit 7"},
	}, nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if strings.TrimSpace(so) != "to-stdout" {
		t.Errorf("stdout = %q, want %q", so, "to-stdout")
	}
	if strings.TrimSpace(se) != "to-stderr" {
		t.Errorf("stderr = %q, want %q", se, "to-stderr")
	}
	if code != 7 {
		t.Errorf("exit code = %d, want 7 (a non-zero exit is a result, not an error)", code)
	}
}

func TestEngineExecHandlesMultiFrameOutput(t *testing.T) {
	e, cid := liveContainer(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	const lines = 5000
	so, _, code, err := e.Exec(ctx, cid, execConfig{
		Cmd: []string{"/bin/sh", "-c", "i=0; while [ $i -lt 5000 ]; do echo line-$i; i=$((i+1)); done"},
	}, nil)
	if err != nil || code != 0 {
		t.Fatalf("exec: err=%v code=%d", err, code)
	}
	got := strings.Count(so, "\n")
	if got != lines {
		t.Errorf("got %d lines, want %d — frames were dropped or merged", got, lines)
	}
	if strings.ContainsRune(so, 0) {
		t.Error("output contains NUL bytes — frame headers leaked into the payload")
	}
	if !strings.Contains(so, "line-4999") {
		t.Error("the tail of the output is missing")
	}
}

func TestEngineExecPassesEnvAndWorkdir(t *testing.T) {
	e, cid := liveContainer(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	so, _, code, err := e.Exec(ctx, cid, execConfig{
		Cmd:        []string{"/bin/sh", "-c", "echo $MARKER; pwd"},
		Env:        []string{"MARKER=hello-env"},
		WorkingDir: "/tmp",
	}, nil)
	if err != nil || code != 0 {
		t.Fatalf("exec: err=%v code=%d", err, code)
	}
	if !strings.Contains(so, "hello-env") {
		t.Errorf("env not passed: %q", so)
	}
	if !strings.Contains(so, "/tmp") {
		t.Errorf("working dir not applied: %q", so)
	}
}

func TestEngineExecStreamsStdin(t *testing.T) {
	e, cid := liveContainer(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	const payload = "written-through-stdin\n"
	_, se, code, err := e.Exec(ctx, cid, execConfig{
		Cmd: []string{"/bin/sh", "-c", "cat > /tmp/from-stdin"},
	}, bytes.NewReader([]byte(payload)))
	if err != nil || code != 0 {
		t.Fatalf("write exec: err=%v code=%d stderr=%q", err, code, se)
	}
	so, _, code, err := e.Exec(ctx, cid, execConfig{Cmd: []string{"cat", "/tmp/from-stdin"}}, nil)
	if err != nil || code != 0 {
		t.Fatalf("read back: err=%v code=%d", err, code)
	}
	if so != payload {
		t.Errorf("round-tripped %q, want %q", so, payload)
	}
}

// With one-shot=true there is no previous sample, so CPU% is always 0.
func TestEngineStatsReportsUsage(t *testing.T) {
	e, cid := liveContainer(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	go func() {
		bg, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _, _, _ = e.Exec(bg, cid, execConfig{
			Cmd: []string{"/bin/sh", "-c", "end=$(( $(date +%s) + 8 )); while [ $(date +%s) -lt $end ]; do :; done"},
		}, nil)
	}()
	time.Sleep(2 * time.Second)

	cpu, mem, err := e.Stats(ctx, cid)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if mem <= 0 {
		t.Errorf("memory = %d MiB, want > 0", mem)
	}
	if cpu <= 0 {
		t.Errorf("cpu = %.2f%%, want > 0 while a busy loop runs — a zero here is the one-shot trap", cpu)
	}
	if cpu > 10000 {
		t.Errorf("cpu = %.2f%% is implausible; the percentage calculation is wrong", cpu)
	}
}

func TestEngineTopAndInspectSize(t *testing.T) {
	e, cid := liveContainer(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	titles, procs, err := e.Top(ctx, cid, "-eo pid,comm")
	if err != nil {
		t.Fatalf("top: %v", err)
	}
	if len(titles) < 2 || len(procs) == 0 {
		t.Errorf("top returned titles=%v procs=%d, want columns and at least one process", titles, len(procs))
	}

	if _, _, code, err := e.Exec(ctx, cid, execConfig{
		Cmd: []string{"/bin/sh", "-c", "dd if=/dev/zero of=/tmp/blob bs=1M count=8 2>/dev/null"},
	}, nil); err != nil || code != 0 {
		t.Fatalf("write blob: err=%v code=%d", err, code)
	}
	size, err := e.InspectSize(ctx, cid)
	if err != nil {
		t.Fatalf("inspect size: %v", err)
	}
	if size < 8<<20 {
		t.Errorf("SizeRw = %d bytes, want at least the 8 MiB just written", size)
	}
}

func TestEngineErrorsOnMissingContainer(t *testing.T) {
	e := newEngine()
	if e == nil {
		t.Skip("no reachable docker socket")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if _, _, _, err := e.Exec(ctx, "definitely-not-a-container", execConfig{Cmd: []string{"true"}}, nil); err == nil {
		t.Error("want an error for a missing container")
	}
}
