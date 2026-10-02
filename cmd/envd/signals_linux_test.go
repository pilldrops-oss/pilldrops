//go:build linux

package main

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The helpers are the test binary re-run as envd is run in the guest: the
// server as a background job of sh, which starts it with SIGINT and SIGQUIT
// ignored, and the reaper as PID 1.
func TestMain(m *testing.M) {
	if os.Getenv("ENVD_FOREGROUND_HELPER") == "1" {
		unignoreInterrupt()
		time.Sleep(10 * time.Second)
		os.Exit(0)
	}
	if os.Getenv("ENVD_REAP_HELPER") == "1" {
		reapForever(strings.Fields(os.Getenv("ENVD_REAP_SERVER")))
	}
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

// init starts a child and execs into the reaper; a child that is then killed
// must not linger as a zombie of PID 1.
func TestReaperCollectsAnOrphanThatExits(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pid1 := exec.Command("/bin/sh", "-c", `sleep 300 & s=$!; (sleep 0.2; kill -9 $s) & exec "$0"`, self)
	pid1.Env = append(os.Environ(), "ENVD_REAP_HELPER=1")
	pid1.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWPID}
	if err := pid1.Start(); err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skip("a pid namespace needs CAP_SYS_ADMIN")
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pid1.Process.Kill(); _ = pid1.Wait() })

	var left []string
	seen := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		left = childrenOf(pid1.Process.Pid)
		if len(left) > 0 {
			seen = true
		} else if seen {
			return
		}
	}
	if !seen {
		t.Fatal("PID 1 never started its child; the test proves nothing")
	}
	t.Fatalf("PID 1 still has children after the kill: %v", left)
}

// `pkill envd` killed the server and nothing started it again: exec answered
// 503 for good while the sandbox said running.
func TestReaperStartsTheServerAgainWhenItExits(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	reaper := exec.Command(self)
	reaper.Env = append(os.Environ(), "ENVD_REAP_HELPER=1", "ENVD_REAP_SERVER=/bin/sleep 300")
	if err := reaper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reaper.Process.Kill(); _ = reaper.Wait() })

	server := func() int {
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			for _, c := range childrenOf(reaper.Process.Pid) {
				if pid, _ := strconv.Atoi(strings.Fields(c)[0]); pid > 0 && !strings.HasSuffix(c, " Z") {
					return pid
				}
			}
		}
		return 0
	}
	first := server()
	if first == 0 {
		t.Fatal("the reaper never started its server")
	}
	if err := syscall.Kill(first, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	second := server()
	if second == 0 || second == first {
		t.Fatalf("server after the kill = %d (first %d); it was not started again", second, first)
	}
}

// childrenOf returns "pid state" for each process whose parent is ppid.
func childrenOf(ppid int) []string {
	dirs, _ := os.ReadDir("/proc")
	var out []string
	for _, d := range dirs {
		b, err := os.ReadFile("/proc/" + d.Name() + "/stat")
		if err != nil {
			continue
		}
		// The comm field may hold spaces; the fields after its closing paren do not.
		f := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+1:]))
		if len(f) > 1 && f[1] == strconv.Itoa(ppid) {
			out = append(out, d.Name()+" "+f[0])
		}
	}
	return out
}

// The reaper is PID 1: a signal that ends it panics the guest.
func TestReaperSurvivesTerminatingSignals(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	reaper := exec.Command(self)
	reaper.Env = append(os.Environ(), "ENVD_REAP_HELPER=1")
	if err := reaper.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- reaper.Wait() }()
	t.Cleanup(func() { _ = reaper.Process.Kill() })

	time.Sleep(200 * time.Millisecond) // past the runtime's own handler install
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGUSR1} {
		if err := reaper.Process.Signal(sig); err != nil {
			t.Fatalf("send %v: %v", sig, err)
		}
		select {
		case err := <-exited:
			t.Fatalf("the reaper exited on %v: %v", sig, err)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Run in the foreground, as in local dev, Ctrl-C must still stop envd.
func TestForegroundEnvdStopsOnInterrupt(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	envd := exec.Command(self)
	envd.Env = append(os.Environ(), "ENVD_FOREGROUND_HELPER=1")
	if err := envd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- envd.Wait() }()
	t.Cleanup(func() { _ = envd.Process.Kill() })

	time.Sleep(200 * time.Millisecond)
	if err := envd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("a foreground envd ignored Ctrl-C")
	}
}
