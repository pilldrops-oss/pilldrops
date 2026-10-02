package docker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// A docker capture is a committed image plus a file naming it, and the image is
// not in the capture directory. Externalize is what puts it there; without it a
// backup that leaves this node restores to "checkpoint image is gone".
//
// This runs real `docker save` and `docker load`, because the failure being
// guarded against is precisely that those two do not round-trip the tag — which
// a mock could not tell you.
func TestExternalizeMakesACaptureSelfContained(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI unavailable")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("no reachable docker daemon")
	}

	// A container to commit, so the image under test is one this code made.
	out, err := exec.Command("docker", "run", "-d", "alpine:3", "sleep", "60").Output()
	if err != nil {
		t.Skipf("cannot start a container: %v", err)
	}
	cid := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", cid).Run() })

	const sandboxID, name = "sb-portable-test", "bkp-1"
	tag := ckptImageTag(sandboxID, name)
	t.Cleanup(func() { _ = exec.Command("docker", "rmi", "-f", tag).Run() })
	if err := exec.Command("docker", "commit", cid, tag).Run(); err != nil {
		t.Skipf("cannot commit: %v", err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ckptTagFile), []byte(tag), 0o600); err != nil {
		t.Fatal(err)
	}

	r := &Runtime{}
	if err := r.Externalize(t.Context(), dir); err != nil {
		t.Fatalf("Externalize: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, ckptImageFile))
	if err != nil {
		t.Fatalf("no saved image in the capture directory: %v", err)
	}
	if info.Size() < 1024 {
		t.Fatalf("the saved image is %d bytes; that is not an image", info.Size())
	}

	// Now make the node forget the image, which is what moving to another host
	// amounts to.
	if err := exec.Command("docker", "rmi", "-f", tag).Run(); err != nil {
		t.Fatalf("could not drop the image: %v", err)
	}
	if exec.Command("docker", "image", "inspect", tag).Run() == nil {
		t.Fatal("the image is still here, so the rest of this test proves nothing")
	}

	if err := r.Internalize(t.Context(), dir); err != nil {
		t.Fatalf("Internalize: %v", err)
	}
	if err := exec.Command("docker", "image", "inspect", tag).Run(); err != nil {
		t.Fatalf("after Internalize the checkpoint image %s is still missing: %v", tag, err)
	}
}

// A capture directory with no saved image is one that never left this node, so
// Internalize has nothing to do and must not fail the restore.
func TestInternalizeIsANoOpWithoutASavedImage(t *testing.T) {
	r := &Runtime{}
	if err := r.Internalize(t.Context(), t.TempDir()); err != nil {
		t.Fatalf("Internalize on a local capture = %v, want nil", err)
	}
}

// Externalize needs the tag file; without it there is nothing to save and
// carrying on would produce a capture that silently restores to nothing.
func TestExternalizeRefusesACaptureWithNoTag(t *testing.T) {
	r := &Runtime{}
	if err := r.Externalize(t.Context(), t.TempDir()); err == nil {
		t.Fatal("Externalize accepted a directory with no tag file")
	}
}

var _ rt.PortableCapture = (*Runtime)(nil)

// A paused docker sandbox is its committed pause image: the backup of one tags
// that image as a checkpoint rather than starting the sandbox.
func TestLiveCheckpointPausedTagsThePauseImage(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Skipf("docker runtime unavailable: %v", err)
	}
	ctx := t.Context()
	id := liveSandboxID(t, "sb-live-pausedckpt")
	inst, err := r.Boot(ctx, rt.BootSpec{SandboxID: id, Template: rt.Template{Name: "base", Image: "alpine:3"}, VCPU: 1, MemMiB: 256})
	if err != nil {
		t.Skipf("boot: %v", err)
	}
	t.Cleanup(func() { _ = r.DeleteSnapshot(context.Background(), id, rt.SnapshotRef{}) })
	snap := rt.SnapshotRef{Path: filepath.Join(t.TempDir(), "snap")}
	if err := inst.Snapshot(ctx, snap); err != nil {
		t.Fatalf("pause snapshot: %v", err)
	}
	if err := inst.Stop(ctx); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if _, err := r.CheckpointPaused(ctx, id, snap, rt.SnapshotRef{Path: dir, Name: "bkp-1"}); err != nil {
		t.Fatalf("CheckpointPaused: %v", err)
	}
	tag, err := os.ReadFile(filepath.Join(dir, ckptTagFile))
	if err != nil || strings.TrimSpace(string(tag)) != ckptImageTag(id, "bkp-1") {
		t.Fatalf("tag file = %q, %v", tag, err)
	}
	if err := exec.Command("docker", "image", "inspect", string(tag)).Run(); err != nil {
		t.Fatalf("the checkpoint image %s does not exist: %v", tag, err)
	}
}
