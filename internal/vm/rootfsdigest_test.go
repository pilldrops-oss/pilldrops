//go:build linux

package vm

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// managerWithRootfs returns a manager whose SnapshotDir holds one template image.
func managerWithRootfs(t *testing.T, template string, content []byte) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, template), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, template, "rootfs.squashfs")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write rootfs: %v", err)
	}
	return &Manager{config: Config{SnapshotDir: dir}, log: zap.NewNop()}, path
}

// The refusal itself: a capture taken against other bytes under the same
// template name must not restore here. It is the whole change — before it, this
// case returned a running sandbox whose every binary read EIO.
func TestCheckRootfsRefusesACaptureFromAnotherNodesImage(t *testing.T) {
	m, _ := managerWithRootfs(t, "base", []byte("this node's build of base"))
	mine, err := m.rootfsDigest("base")
	if err != nil {
		t.Fatalf("rootfsDigest: %v", err)
	}

	other, _ := managerWithRootfs(t, "base", []byte("the OTHER node's build of base"))
	theirs, err := other.rootfsDigest("base")
	if err != nil {
		t.Fatalf("rootfsDigest: %v", err)
	}
	if mine == theirs {
		t.Fatal("two different images hashed the same; this test cannot tell the cases apart")
	}

	err = m.checkRootfs(sandboxSnapMeta{Template: "base", RootfsDigest: theirs}, "/snap/x", "")
	if !errors.Is(err, ErrRootfsMismatch) {
		t.Fatalf("checkRootfs on a foreign image = %v, want ErrRootfsMismatch", err)
	}
	// The operator has to be able to act on it, so both digests are named.
	if msg := err.Error(); !strings.Contains(msg, theirs) || !strings.Contains(msg, mine) {
		t.Errorf("the refusal names neither what was captured nor what is here: %q", msg)
	}

	// Same node, same image: the path that has always worked keeps working.
	if err := m.checkRootfs(sandboxSnapMeta{Template: "base", RootfsDigest: mine}, "/snap/x", ""); err != nil {
		t.Errorf("checkRootfs on this node's own image = %v, want nil", err)
	}
}

// A capture from before the digest existed still restores; refusing those would
// strand every snapshot taken to date on the node that took it.
func TestCheckRootfsAllowsACaptureWithNoDigest(t *testing.T) {
	m, _ := managerWithRootfs(t, "base", []byte("x"))
	if err := m.checkRootfs(sandboxSnapMeta{Template: "base"}, "/snap/x", ""); err != nil {
		t.Fatalf("checkRootfs with no recorded digest = %v, want nil", err)
	}
}

// The trap this check exists to avoid repeating: a failure to read must never
// be indistinguishable from a pass. A recorded digest with no local image to
// compare it against is an error, not a silent yes.
func TestCheckRootfsFailsWhenTheLocalImageCannotBeRead(t *testing.T) {
	m, path := managerWithRootfs(t, "base", []byte("x"))
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := m.checkRootfs(sandboxSnapMeta{Template: "base", RootfsDigest: "sha256:deadbeef"}, "/snap/x", ""); err == nil {
		t.Fatal("checkRootfs passed with no local image to compare against")
	}
}

// The memo must key on the bytes, not the name: a rebuilt image of identical
// size is exactly the case a cache keyed on the path alone would get wrong, and
// it is the case that matters (every node rebuilds base from the Dockerfile).
func TestRootfsDigestFollowsARebuiltImageOfTheSameSize(t *testing.T) {
	m, path := managerWithRootfs(t, "base", []byte("build-one"))
	first, err := m.rootfsDigest("base")
	if err != nil {
		t.Fatalf("rootfsDigest: %v", err)
	}
	if err := os.WriteFile(path, []byte("build-two"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	later := time.Now().Add(time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	second, err := m.rootfsDigest("base")
	if err != nil {
		t.Fatalf("rootfsDigest: %v", err)
	}
	if first == second {
		t.Fatal("the digest did not change when the image did; the memo is serving a stale answer")
	}
}

// The second half of the fix: a capture that CARRIES its own image is checked
// and booted against that image, not against whatever this node built under the
// same template name. Without this, the node that fetched a snapshot would
// refuse every one of them — its own base always differs.
func TestACarriedImageIsWhatIsCheckedNotTheLocalTemplate(t *testing.T) {
	m, _ := managerWithRootfs(t, "base", []byte("THIS node's build of base"))
	local, err := m.rootfsDigest("base")
	if err != nil {
		t.Fatalf("rootfsDigest: %v", err)
	}

	// The fetched capture directory, with the capturing node's image beside the
	// other objects, exactly as fetchSnapshot lands it.
	snap := t.TempDir()
	carried := []byte("the CAPTURING node's build of base")
	if err := os.WriteFile(filepath.Join(snap, "rootfs.squashfs"), carried, 0o644); err != nil {
		t.Fatalf("write carried rootfs: %v", err)
	}
	path := captureRootfs(snap)
	if path == "" {
		t.Fatal("captureRootfs did not find the image the capture carries")
	}
	captured, err := m.digestOf(path)
	if err != nil {
		t.Fatalf("digestOf: %v", err)
	}
	if captured == local {
		t.Fatal("the carried image and this node's are the same bytes; the test proves nothing")
	}

	meta := sandboxSnapMeta{Template: "base", RootfsDigest: captured}
	if err := m.checkRootfs(meta, snap, path); err != nil {
		t.Fatalf("checkRootfs against the carried image = %v, want nil", err)
	}
	// And the same capture without its image still refuses, which is what makes
	// the pass above a property of the image travelling rather than of the check
	// having been softened.
	if err := m.checkRootfs(meta, snap, ""); !errors.Is(err, ErrRootfsMismatch) {
		t.Fatalf("checkRootfs falling back to the local template = %v, want ErrRootfsMismatch", err)
	}
}

// A capture directory with no image of its own reports none. Pins the fallback:
// a local pause or checkpoint has no rootfs.squashfs beside its state and mem,
// and must keep resolving through the template name.
func TestCaptureRootfsIsEmptyForACaptureThatCarriesNoImage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mem"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := captureRootfs(dir); got != "" {
		t.Fatalf("captureRootfs = %q, want \"\" for a capture with no image", got)
	}
}
