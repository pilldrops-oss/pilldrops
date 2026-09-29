//go:build linux

package vm

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestStopVMMBoundedKillsASigtermIgnorer(t *testing.T) {
	cmd := exec.Command("sh", "-c", `trap "" TERM; sleep 60`)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	reaped := make(chan struct{})
	go func() { _ = cmd.Wait(); close(reaped) }()
	// Let the shell install the trap before SIGTERM lands.
	time.Sleep(150 * time.Millisecond)

	start := time.Now()
	err := stopVMMBounded(pid, func() error { return syscall.Kill(pid, syscall.SIGTERM) })
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("stopVMMBounded: %v", err)
	}
	if elapsed > 4*time.Second {
		t.Fatalf("took %v; the whole point is a bounded stop (2s grace + SIGKILL)", elapsed)
	}
	select {
	case <-reaped:
	case <-time.After(2 * time.Second):
		t.Fatal("SIGTERM-ignoring process still running after stopVMMBounded returned success")
	}
}

func TestStopVMMBoundedIsQuickWhenSigtermWorks(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()

	start := time.Now()
	if err := stopVMMBounded(pid, func() error { return syscall.Kill(pid, syscall.SIGTERM) }); err != nil {
		t.Fatalf("stopVMMBounded: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("a SIGTERM-compliant process took %v to stop; the grace poll should see it go in ~25ms", elapsed)
	}
}
