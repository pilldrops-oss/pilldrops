//go:build linux

package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// The helper is the test binary re-run as envd is run in the guest: as a
// background job of sh, which starts it with SIGINT and SIGQUIT ignored.
func TestMain(m *testing.M) {
	if os.Getenv("ENVD_SIGNAL_HELPER") == "1" {
		unignoreInterrupt()
		cmd := exec.Command("/bin/sh", "-c", "grep SigIgn /proc/self/status")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Stdout = os.Stdout
		if err := cmd.Run(); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestChildrenOfABackgroundedEnvdGetDefaultInterruptAndQuit(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	helper := exec.Command("/bin/sh", "-c", `"$0" & wait`, self)
	helper.Env = append(os.Environ(), "ENVD_SIGNAL_HELPER=1")
	out, err := helper.Output()
	if err != nil {
		t.Fatalf("helper: %v", err)
	}
	field := strings.Fields(strings.TrimPrefix(strings.TrimSpace(string(out)), "SigIgn:"))
	if len(field) != 1 {
		t.Fatalf("unexpected status line %q", out)
	}
	ignored, err := strconv.ParseUint(field[0], 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGQUIT} {
		if ignored&(1<<(uint(sig)-1)) != 0 {
			t.Errorf("child inherited %v as ignored (SigIgn %s)", sig, field[0])
		}
	}
}
