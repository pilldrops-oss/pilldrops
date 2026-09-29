//go:build linux

package vm

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"go.uber.org/zap"
)

// needRoot skips a test that seals a file: sealGolden chowns to root and fails
// closed when it cannot, as the daemon, which always runs as root, should.
// scripts/check.sh runs these as root in a linux container.
func needRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("sealGolden chowns to root; run as root")
	}
}

// Firecracker writes the golden mem as the golden VM's unprivileged uid, a uid
// uidForSandbox can later hand to a tenant.
func TestSealGoldenLeavesNoAllocatableUIDAsOwner(t *testing.T) {
	needRoot(t)
	path := filepath.Join(t.TempDir(), "mem")
	if err := os.WriteFile(path, []byte("golden memory"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Reproduce the broken state: 0644 and, when root, the golden uid as owner.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	goldenUID := uidForSandbox("golden-base-31ac750e")
	if os.Geteuid() == 0 {
		if err := os.Chown(path, goldenUID, goldenUID); err != nil {
			t.Fatal(err)
		}
	}

	if err := sealGolden(path); err != nil {
		t.Fatal(err)
	}

	if os.Geteuid() == 0 {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		uid := ownerUID(t, st)
		if uid != 0 {
			t.Fatalf("golden mem is owned by uid %d, want 0. uidForSandbox draws from "+
				"[10000,59999], so a non-root owner here is a uid this node can hand to "+
				"a later sandbox — whose VMM would then own the memory image EVERY "+
				"sandbox on the node restores into", uid)
		}
	}
	if got := modeOf(t, path); got != goldenSealedMode {
		t.Fatalf("mode is %04o, want %04o", got, goldenSealedMode)
	}
}

// Every restoring VMM is a different unprivileged uid and must still read it.
func TestSealGoldenKeepsItReadableByEveryVMMUID(t *testing.T) {
	needRoot(t)
	path := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(path, []byte("state"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sealGolden(path); err != nil {
		t.Fatal(err)
	}
	if got := modeOf(t, path); got&0o444 != 0o444 {
		t.Fatalf("mode %04o: a restoring VMM running as some other uid cannot read the "+
			"golden snapshot, so no sandbox on this node can be restored", got)
	}
}

// 0444 rather than 0644 so the invariant survives a later chown by hand.
func TestSealGoldenGrantsWriteToNobody(t *testing.T) {
	needRoot(t)
	path := filepath.Join(t.TempDir(), "mem")
	if err := os.WriteFile(path, []byte("golden"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o666); err != nil { // umask-proof
		t.Fatal(err)
	}
	if err := sealGolden(path); err != nil {
		t.Fatal(err)
	}
	if got := modeOf(t, path); got&0o222 != 0 {
		t.Fatalf("mode %04o still carries a write bit", got)
	}
}

// state alone redirects a restore and mem alone plants code in it.
func TestSealGoldenDirCoversStateAndMem(t *testing.T) {
	needRoot(t)
	dir := t.TempDir()
	for _, n := range []string{"state", "mem"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(dir, n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := sealGoldenDir(dir); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"state", "mem"} {
		if got := modeOf(t, filepath.Join(dir, n)); got != goldenSealedMode {
			t.Fatalf("%s is %04o, want %04o", n, got, goldenSealedMode)
		}
	}
}

func TestSealGoldenFailsClosedOnAMissingFile(t *testing.T) {
	if err := sealGolden(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("sealing a missing golden file was accepted")
	}
	if err := sealGoldenDir(t.TempDir()); err == nil {
		t.Fatal("sealing a directory with no state/mem was accepted")
	}
}

// Skipping an unsealable version is not enough: RestoreFromSnapshot does its
// own snapshotValid check and would still serve it. Exercised by re-executing
// this test as an unprivileged uid so the chown fails for real.
func TestValidateRefusesToServeAGoldenItCannotSeal(t *testing.T) {
	const childEnv = "PILLDROPS_GOLDENSEAL_CHILD"

	if dir := os.Getenv(childEnv); dir != "" {
		// Child, running unprivileged.
		if os.Geteuid() == 0 {
			t.Fatalf("the child is still root (euid 0); the chown would succeed and " +
				"this test would pass without exercising anything")
		}
		cfg := Config{SnapshotDir: dir}
		err := ValidateOrCreateSnapshot("base", cfg, zap.NewNop())
		if err == nil {
			t.Fatalf("ValidateOrCreateSnapshot accepted a golden snapshot it could not "+
				"seal (euid %d). RestoreFromSnapshot would then serve every sandbox from "+
				"an image an allocatable uid owns", os.Geteuid())
		}
		if !strings.Contains(err.Error(), "refusing to serve") {
			t.Fatalf("error is %q; it must name the refusal, or an operator reads it as "+
				"an unrelated startup failure", err)
		}
		return
	}

	if os.Geteuid() != 0 {
		t.Log("not root: cannot drop privilege, so this runs in-process and only " +
			"asserts the happy path stays happy")
	}

	// Not t.TempDir(): its 0700 parent stops an unprivileged child before it
	// reaches the code under test, which looks identical to the refusal.
	work, err := os.MkdirTemp("", "goldenseal")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(work) })
	if err := os.Chmod(work, 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(work, "templates")
	snap := filepath.Join(root, "base", "snapshots", "v1")
	if err := os.MkdirAll(snap, 0o755); err != nil {
		t.Fatal(err)
	}
	// World-readable so the child gets as far as the seal.
	for _, n := range []string{"state", "mem", goldenOverlayFile} {
		if err := os.WriteFile(filepath.Join(snap, n), []byte("fixture"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeGoldenIdentity(snap, goldenIdentity{
		TapName: "fctap9", VMIP: "192.168.0.9", OverlayBase: "golden.ext4",
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(snap, goldenIdentityFile), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{root, filepath.Join(root, "base"),
		filepath.Join(root, "base", "snapshots"), snap} {
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if !snapshotValid(snap) {
		t.Fatal("the fixture is not a valid snapshot, so the child would return early " +
			"and pass for the wrong reason")
	}

	if os.Geteuid() != 0 {
		// Already unprivileged: run the assertion directly.
		if err := ValidateOrCreateSnapshot("base", Config{SnapshotDir: root}, zap.NewNop()); err == nil {
			t.Fatal("ValidateOrCreateSnapshot accepted a golden it could not seal")
		}
		return
	}

	// Re-exec as a sandbox-range uid. The binary is copied out of go-build's
	// 0700 tree first, or the child fails to exec and that looks like the assertion firing.
	const asUID = 12345
	self, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(work, "child.test")
	if err := os.WriteFile(child, self, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(child, "-test.run=TestValidateRefusesToServeAGoldenItCannotSeal", "-test.v")
	cmd.Env = append(os.Environ(), childEnv+"="+root)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: asUID, Gid: asUID},
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the unprivileged child failed (%v):\n%s", err, out)
	}
	if !strings.Contains(string(out), "PASS") {
		t.Fatalf("the child did not report PASS, so nothing was asserted:\n%s", out)
	}
	// Control: the same seal must succeed as root.
	if err := sealGoldenDir(snap); err != nil {
		t.Fatalf("CONTROL FAILED: root could not seal the same fixture (%v), so the "+
			"child's error was not about privilege at all", err)
	}
}

// scripts/security-suite.sh hardcodes the sandbox uid range; pin it to uidForSandbox.
func TestSecuritySuitePinsTheSandboxUIDRange(t *testing.T) {
	const script = "../../scripts/security-suite.sh"
	b, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)

	num := func(name string) int {
		t.Helper()
		m := regexp.MustCompile(`(?m)^` + name + `=(\d+)$`).FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("%s no longer defines %s. §7's owner check cannot tell an allocatable "+
				"uid from a system one without it", script, name)
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	lo, hi := num("SB_UID_LO"), num("SB_UID_HI")

	// Derived by exercising uidForSandbox, since a copy of its formula would agree with a broken one.
	gotLo, gotHi := 1<<31-1, 0
	for i := 0; i < 400000; i++ {
		u := uidForSandbox(fmt.Sprintf("sb-%d-%x", i, i*2654435761))
		if u < gotLo {
			gotLo = u
		}
		if u > gotHi {
			gotHi = u
		}
	}
	if gotLo < lo || gotHi > hi {
		t.Fatalf("uidForSandbox produces [%d,%d] but %s checks [%d,%d]. Every uid outside "+
			"the script's range is one it would call 'not a sandbox uid' while the node "+
			"hands it to a sandbox", gotLo, gotHi, script, lo, hi)
	}
	if lo == 0 {
		t.Fatalf("%s has SB_UID_LO=0, which makes root look allocatable and the whole "+
			"check vacuous", script)
	}
}

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func ownerUID(t *testing.T, fi os.FileInfo) int {
	t.Helper()
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("no syscall.Stat_t for %s — the ownership assertion cannot run", fi.Name())
	}
	return int(st.Uid)
}
