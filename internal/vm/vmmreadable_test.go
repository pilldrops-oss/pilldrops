//go:build linux

package vm

import (
	"os"
	"path/filepath"
	"testing"
)

// The guest kernel ships 0600 root:root and the jailer chowns only what it
// creates, so an unprivileged VMM could not read its own kernel image.
func TestEnsureVMMReadableWidensAnUnreadableKernel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vmlinux")
	if err := os.WriteFile(path, []byte("kernel"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := ensureVMMReadable(path); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got&0o044 != 0o044 {
		t.Fatalf("mode is %04o: an unprivileged VMM still cannot read its kernel", got)
	}
}

// Group/other write on a file every chroot hard-links would let any sandbox rewrite the shared kernel.
func TestEnsureVMMReadableGrantsNoWriteOrExecute(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rootfs.squashfs")
	if err := os.WriteFile(path, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureVMMReadable(path); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got&0o033 != 0 {
		t.Fatalf("mode is %04o: group/other gained write or execute on a file shared "+
			"by every sandbox on the node", got)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Fatalf("mode is %04o, want 0644 (0600 plus group+other read)", got)
	}
}

func TestEnsureVMMReadableLeavesAReadableFileAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rootfs.squashfs")
	if err := os.WriteFile(path, []byte("image"), 0o640); err != nil {
		t.Fatal(err)
	}
	// 0640 is not other-readable, and the VMM's uid is in no group of ours.
	if err := ensureVMMReadable(path); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Fatalf("0640 → %04o, want 0644", got)
	}

	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureVMMReadable(path); err != nil {
		t.Fatal(err)
	}
	fi, _ = os.Stat(path)
	if got := fi.Mode().Perm(); got != 0o755 {
		t.Fatalf("an already-readable 0755 file was changed to %04o", got)
	}
}

func TestPrepareSharedVMMInputsIsANoOpForARootVMM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vmlinux")
	if err := os.WriteFile(path, []byte("kernel"), 0o600); err != nil {
		t.Fatal(err)
	}

	fc := &FirecrackerConfig{SandboxID: "sb-test-0001"}
	fc.Config.NonRootVMM = false
	if err := prepareSharedVMMInputs(fc, path); err != nil {
		t.Fatal(err)
	}

	fi, _ := os.Stat(path)
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("root VMM path changed the kernel mode to %04o, want 0600 untouched", got)
	}
}

func TestPrepareSharedVMMInputsFailsClosedOnAMissingFile(t *testing.T) {
	fc := &FirecrackerConfig{SandboxID: "sb-test-0001"}
	fc.Config.NonRootVMM = true
	if err := prepareSharedVMMInputs(fc, filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("a missing kernel image was accepted; the boot would proceed and fail later")
	}
}
