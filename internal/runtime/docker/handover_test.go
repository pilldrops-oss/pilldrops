package docker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// handoverDocker answers `docker exec ... [ -e <mark> ]` from a file: absent
// is "not yet", "dead" is a container that exited.
const handoverDocker = `#!/bin/sh
[ -f "$FAKE_DOCKER_DIR/dead" ] && { echo "Error: container c1 is not running" >&2; exit 1; }
[ -f "$FAKE_DOCKER_DIR/done" ]
`

func fakeHandoverDocker(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(handoverDocker), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_DOCKER_DIR", dir)
	return dir
}

// docker run returns once PID 1 starts, before its chown of the workspace; a
// write as the run user right after Boot then failed with EACCES (seen in CI).
func TestBootWaitsForTheWorkspaceHandover(t *testing.T) {
	dir := fakeHandoverDocker(t)
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = os.WriteFile(filepath.Join(dir, "done"), nil, 0o644)
	}()
	start := time.Now()
	if err := waitForHandover(t.Context(), "c1", "1000:1000", "/.pd-handover-x"); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if waited := time.Since(start); waited < 300*time.Millisecond {
		t.Errorf("returned after %v, before the handover finished", waited)
	}
	if !strings.Contains(sourceContaining(t, "func (r *Runtime) bootImage("), "waitForHandover(ctx, cid, runUser, handoverDone)") {
		t.Error("bootImage does not wait for the handover; Boot can return before the workspace is the run user's")
	}
}

func TestBootFailsWhenTheContainerDiesBeforeTheHandover(t *testing.T) {
	dir := fakeHandoverDocker(t)
	if err := os.WriteFile(filepath.Join(dir, "dead"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	defer func(d time.Duration) { handoverTimeout = d }(handoverTimeout)
	handoverTimeout = 5 * time.Second
	start := time.Now()
	err := waitForHandover(t.Context(), "c1", "1000:1000", "/.pd-handover-x")
	if err == nil || !strings.Contains(err.Error(), "exited before handing over") {
		t.Fatalf("err = %v, want the container's exit reported", err)
	}
	if time.Since(start) > time.Second {
		t.Error("waited out the timeout for a container that had already exited")
	}
}
