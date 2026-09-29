package runtimetest

import (
	"context"
	"testing"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

func TestFakeImplementsRuntime(t *testing.T) {
	var _ rt.Runtime = NewFake()
}

func TestFakeExecAndFS(t *testing.T) {
	f := NewFake()
	inst, err := f.Boot(context.Background(), rt.BootSpec{SandboxID: "sb1"})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	if inst.ID() != "sb1" {
		t.Fatalf("id = %q, want sb1", inst.ID())
	}
	if err := inst.FS().Write(t.Context(), "a.txt", []byte("hi")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := inst.FS().Read(t.Context(), "a.txt")
	if err != nil || string(got) != "hi" {
		t.Fatalf("read = %q, %v", got, err)
	}
	res, err := inst.Exec(context.Background(), rt.ExecSpec{Cmd: "echo hi"})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit = %d, want 0", res.ExitCode)
	}
	if len(f.ExecLog("sb1")) != 1 {
		t.Fatalf("exec log = %d, want 1", len(f.ExecLog("sb1")))
	}
}
