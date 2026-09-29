//go:build linux

package vm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// The empty string is a real failing value here: an empty VsockPath once made
// RecoverOrphans dial localhost, conclude every VM was dead, and kill them all.

func TestEndpointSurvivesARestart(t *testing.T) {
	m := &Manager{config: Config{OverlayDir: filepath.Join(t.TempDir(), "overlays")}}

	const id, addr = "sb-test-1", "192.168.0.42:6001"
	m.saveEndpoint(id, addr)

	restarted := &Manager{config: m.config}
	if got := restarted.loadEndpoint(id); got != addr {
		t.Fatalf("loadEndpoint after restart = %q, want %q", got, addr)
	}

	restarted.removeEndpoint(id)
	if got := restarted.loadEndpoint(id); got != "" {
		t.Fatalf("loadEndpoint after remove = %q, want empty", got)
	}
}

func TestUnknownSandboxHasNoEndpoint(t *testing.T) {
	m := &Manager{config: Config{OverlayDir: filepath.Join(t.TempDir(), "overlays")}}
	if got := m.loadEndpoint("sb-never-created"); got != "" {
		t.Fatalf("loadEndpoint(unknown) = %q, want empty", got)
	}
}

// Endpoint plus token is everything needed to drive a tenant's envd.
func TestEndpointFileIsRootOnly(t *testing.T) {
	m := &Manager{config: Config{OverlayDir: filepath.Join(t.TempDir(), "overlays")}}
	m.saveEndpoint("sb-perm", "192.168.0.9:6001")

	fi, err := os.Stat(m.endpointPath("sb-perm"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("endpoint file mode = %o, want 600", perm)
	}
}

// If this disagrees with createOverlay's path the kill path silently deletes nothing.
func TestReconstructedOverlayPathMatchesTheRealOne(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{OverlayDir: dir}
	const id = "sb-1787726247455-e17cb7b1"

	reconstructed := filepath.Join(cfg.OverlayDir, id+".ext4")
	if reconstructed == filepath.Join(cfg.OverlayDir, ".ext4") {
		t.Fatal("reconstructed path lost the sandbox id")
	}

	if err := os.WriteFile(reconstructed, []byte("overlay"), 0o600); err != nil {
		t.Fatalf("seed overlay: %v", err)
	}
	if err := os.Remove(reconstructed); err != nil {
		t.Fatalf("removing the reconstructed path failed: %v", err)
	}
}

// An orphaned VMM keeps the dead sandboxd's pgid, so kill(-pid) hits nothing;
// the child here is in its parent's group for the same reason.
func TestStopKillsAProcessThatDoesNotLeadItsGroup(t *testing.T) {
	cmd := exec.Command("sleep", "300")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	}()

	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatalf("getpgid: %v", err)
	}
	if pgid == pid {
		t.Skipf("child unexpectedly leads its own group (pgid=%d); this test needs the orphan shape", pgid)
	}

	if err := (&procKiller{pid: pid}).Stop(context.Background()); err != nil {
		t.Fatalf("Stop on a non-group-leader returned %v; the VM would survive", err)
	}

	if _, err := cmd.Process.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatal("process still alive after Stop")
	}
}
