//go:build linux

package vm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCgroupTree builds a cgroup-v2-shaped temp tree with the given controllers
// available and delegated at both levels, and points cgroupRoot at it. It does
// not emulate subtree_control append semantics.
func fakeCgroupTree(t *testing.T, available, delegated string) {
	t.Helper()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "cgroup.controllers"), available)
	mustWrite(t, filepath.Join(root, "cgroup.subtree_control"), delegated)
	fc := filepath.Join(root, "firecracker")
	if err := os.MkdirAll(fc, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(fc, "cgroup.controllers"), available)
	mustWrite(t, filepath.Join(fc, "cgroup.subtree_control"), delegated)

	old := cgroupRoot
	cgroupRoot = root
	t.Cleanup(func() { cgroupRoot = old })
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readTrimmed(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.TrimSpace(string(b))
}

func TestPrepareVMMCgroupWritesAllThreeLimits(t *testing.T) {
	fakeCgroupTree(t, "cpuset cpu io memory hugetlb pids", "cpuset cpu memory pids")

	if err := prepareVMMCgroup("sb-1-abc", 2048, 4); err != nil {
		t.Fatalf("prepareVMMCgroup: %v", err)
	}
	leaf := vmmCgroupPath("sb-1-abc")

	// 2048 MiB guest + 512 MiB overhead.
	if got, want := readTrimmed(t, filepath.Join(leaf, "memory.max")), "2684354560"; got != want {
		t.Errorf("memory.max = %s, want %s", got, want)
	}
	if got, want := readTrimmed(t, filepath.Join(leaf, "pids.max")), "36"; got != want {
		t.Errorf("pids.max = %s, want %s", got, want)
	}
	if got, want := readTrimmed(t, filepath.Join(leaf, "cpu.max")), "450000 100000"; got != want {
		t.Errorf("cpu.max = %s, want %s", got, want)
	}
}

func TestVMMCgroupLimitsTrackTheVMSize(t *testing.T) {
	small := vmmCgroupLimits(512, 1)
	large := vmmCgroupLimits(8192, 8)

	if small.MemoryMaxBytes >= large.MemoryMaxBytes {
		t.Errorf("memory.max does not scale: 512MiB→%d, 8192MiB→%d",
			small.MemoryMaxBytes, large.MemoryMaxBytes)
	}
	if small.PidsMax >= large.PidsMax {
		t.Errorf("pids.max does not scale with vCPUs: %d vs %d", small.PidsMax, large.PidsMax)
	}
	if small.CPUMax == large.CPUMax {
		t.Errorf("cpu.max does not scale with vCPUs: both %q", small.CPUMax)
	}
	// The ceiling must exceed the guest's own RAM or every VM OOM-kills itself.
	if small.MemoryMaxBytes <= 512*1024*1024 {
		t.Errorf("memory.max %d is not above the guest's own 512 MiB", small.MemoryMaxBytes)
	}
}

func TestPrepareVMMCgroupFailsClosedWhenAControllerIsNotDelegated(t *testing.T) {
	// memory is absent from cgroup.controllers, so it cannot be delegated.
	fakeCgroupTree(t, "cpuset cpu io pids", "cpuset cpu pids")

	err := prepareVMMCgroup("sb-1-abc", 1024, 1)
	if err == nil {
		t.Fatal("prepareVMMCgroup returned nil on a host that cannot delegate the memory controller")
	}
	if !strings.Contains(err.Error(), "memory") {
		t.Errorf("error does not name the missing controller: %v", err)
	}
}

func TestPrepareVMMCgroupSkipsOnlyWhenThereIsNoUnifiedHierarchy(t *testing.T) {
	old := cgroupRoot
	cgroupRoot = t.TempDir() // empty: no cgroup.controllers
	t.Cleanup(func() { cgroupRoot = old })

	if err := prepareVMMCgroup("sb-1-abc", 1024, 1); err != nil {
		t.Fatalf("want a silent skip with no unified hierarchy, got %v", err)
	}
	if _, err := os.Stat(vmmCgroupPath("sb-1-abc")); err == nil {
		t.Error("created a cgroup leaf on a host with no cgroup-v2 hierarchy")
	}
}

// The leaf is created empty, not via prepareVMMCgroup: rmdirCgroupTree uses
// rmdir, and a temp tree's plain limit files would block it.
func TestCleanJailerRootReclaimsTheSameLeafPrepareCreated(t *testing.T) {
	fakeCgroupTree(t, "cpuset cpu io memory hugetlb pids", "cpuset cpu memory pids")

	const id = "sb-9-zzz"
	if err := os.MkdirAll(vmmCgroupPath(id), 0o755); err != nil {
		t.Fatal(err)
	}

	chrootBase := t.TempDir()
	chrootRoot := filepath.Join(chrootBase, "firecracker", id, "root")
	if err := os.MkdirAll(chrootRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	cleanJailerRoot(chrootRoot)

	if _, err := os.Stat(vmmCgroupPath(id)); !os.IsNotExist(err) {
		t.Errorf("cleanJailerRoot left the cgroup leaf %s behind (err=%v)", vmmCgroupPath(id), err)
	}
	if _, err := os.Stat(filepath.Dir(chrootRoot)); !os.IsNotExist(err) {
		t.Errorf("cleanJailerRoot left the chroot %s behind", filepath.Dir(chrootRoot))
	}
}
