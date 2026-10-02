package docker

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestProcStartedReadsTheProcessStartTime(t *testing.T) {
	got := procStarted(strconv.Itoa(os.Getpid()))
	if got.IsZero() || time.Since(got) < 0 || time.Since(got) > 10*time.Minute {
		t.Errorf("this test process started at %v, want within the last few minutes", got)
	}
	if !procStarted("not-a-pid").IsZero() {
		t.Error("a missing process has a start time")
	}
}

// A rootless or snap dockerd writes its pidfile elsewhere; without the setting
// its start is unknown and nothing told a restart from a crash.
func TestDockerdStartReadsTheConfiguredPidfile(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "docker.pid")
	if err := os.WriteFile(pidfile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("SANDBOXD_DOCKERD_PIDFILE", pidfile)
	if got, why := dockerdStart(); got.IsZero() || why != "" {
		t.Fatalf("start = %v, %q; want this process's start from %s", got, why, pidfile)
	}

	t.Setenv("SANDBOXD_DOCKERD_PIDFILE", filepath.Join(t.TempDir(), "missing.pid"))
	if got, why := dockerdStart(); !got.IsZero() || !strings.Contains(why, "SANDBOXD_DOCKERD_PIDFILE") {
		t.Fatalf("start = %v, %q; want unknown, naming the setting", got, why)
	}
}

// A local pidfile says nothing about a remote daemon, reached by DOCKER_HOST
// or by a docker context, unless the operator names the pidfile.
func TestRemoteDockerdNeverReadsTheLocalPidfile(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "docker.pid")
	if err := os.WriteFile(pidfile, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeContext(t, home, "remote", "tcp://10.0.0.5:2376")
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "remote")
	t.Setenv("SANDBOXD_DOCKERD_PIDFILE", "")
	if got, why := dockerdStart(); !got.IsZero() || !strings.Contains(why, "not a local socket") {
		t.Fatalf("start = %v, %q; want unknown because the daemon is remote", got, why)
	}
	t.Setenv("SANDBOXD_DOCKERD_PIDFILE", pidfile)
	if got, why := dockerdStart(); got.IsZero() {
		t.Fatalf("start unknown (%q) with the pidfile named; want the setting to win", why)
	}
}
