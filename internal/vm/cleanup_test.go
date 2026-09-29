//go:build linux

package vm

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
)

// "" is ChrootRoot() for a jailer-less machine, e.g. a recovered orphan.
func TestCleanJailerRoot_NoOpOnEmpty(t *testing.T) {
	cleanJailerRoot("")
}

func TestCleanJailerRoot_RemovesJailerDir(t *testing.T) {
	base := t.TempDir()
	old := jailerBaseDir
	jailerBaseDir = base
	t.Cleanup(func() { jailerBaseDir = old })

	// cold-boot shape: <base>/firecracker/<id>/root/...
	chrootRoot := filepath.Join(jailerChrootDir(), "sb-abc123", "root")
	if err := os.MkdirAll(filepath.Join(chrootRoot, "run"), 0o755); err != nil {
		t.Fatal(err)
	}
	cleanJailerRoot(chrootRoot)
	if _, err := os.Stat(filepath.Join(jailerChrootDir(), "sb-abc123")); !os.IsNotExist(err) {
		t.Fatalf("expected jailer dir removed, stat err=%v", err)
	}
}

// Warm-pool jailer ids are "pool-<hex>", not the sandbox id, so cleanJailerRoot
// must derive the dir from the ChrootRoot() path alone.
func TestCleanJailerRoot_PoolHexID(t *testing.T) {
	base := t.TempDir()
	old := jailerBaseDir
	jailerBaseDir = base
	t.Cleanup(func() { jailerBaseDir = old })

	poolID := "pool-deadbeef"
	chrootRoot := filepath.Join(jailerChrootDir(), poolID, "root")
	if err := os.MkdirAll(filepath.Join(chrootRoot, "run"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Siblings under the same jailerChrootDir() must survive.
	siblingRoot := filepath.Join(jailerChrootDir(), "pool-cafef00d", "root")
	if err := os.MkdirAll(siblingRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	cleanJailerRoot(chrootRoot)

	if _, err := os.Stat(filepath.Join(jailerChrootDir(), poolID)); !os.IsNotExist(err) {
		t.Fatalf("expected pool jailer dir removed, stat err=%v", err)
	}
	if _, err := os.Stat(siblingRoot); err != nil {
		t.Fatalf("sibling pool dir must survive, stat err=%v", err)
	}
}

func TestCleanStaleJailer_DelegatesToCleanJailerRoot(t *testing.T) {
	base := t.TempDir()
	old := jailerBaseDir
	jailerBaseDir = base
	t.Cleanup(func() { jailerBaseDir = old })

	id := "sb-restore-0001"
	chrootRoot := filepath.Join(jailerChrootDir(), id, "root")
	if err := os.MkdirAll(chrootRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	cleanStaleJailer(id)

	if _, err := os.Stat(filepath.Join(jailerChrootDir(), id)); !os.IsNotExist(err) {
		t.Fatalf("expected jailer dir removed, stat err=%v", err)
	}
}

// oldModTimes returns modTimes where every id is past chrootSweepGrace.
func oldModTimes(now time.Time, ids ...string) map[string]time.Time {
	m := make(map[string]time.Time, len(ids))
	for _, id := range ids {
		m[id] = now.Add(-chrootSweepGrace - time.Minute)
	}
	return m
}

func TestSweepDecision_SkipsActiveColdBootID(t *testing.T) {
	now := time.Now()
	active := func(id string) bool { return id == "sb-abc123" }
	dead := sweepDecision([]string{"sb-abc123"}, oldModTimes(now, "sb-abc123"), now, map[string]bool{}, active)
	if len(dead) != 0 {
		t.Fatalf("expected active id to be skipped, got dead=%v", dead)
	}
}

// Pool ids are never in m.active; only the live set from liveJailerIDs() protects them.
func TestSweepDecision_SkipsLivePoolHexID(t *testing.T) {
	now := time.Now()
	active := func(string) bool { return false }
	live := map[string]bool{"pool-deadbeef": true}
	dead := sweepDecision([]string{"pool-deadbeef"}, oldModTimes(now, "pool-deadbeef"), now, live, active)
	if len(dead) != 0 {
		t.Fatalf("expected live pool id to be skipped, got dead=%v", dead)
	}
}

func TestSweepDecision_ReturnsUnknownDeadID(t *testing.T) {
	now := time.Now()
	active := func(string) bool { return false }
	live := map[string]bool{}
	dead := sweepDecision([]string{"sb-long-dead-0001"}, oldModTimes(now, "sb-long-dead-0001"), now, live, active)
	if len(dead) != 1 || dead[0] != "sb-long-dead-0001" {
		t.Fatalf("expected the unknown id to be returned as dead, got %v", dead)
	}
}

// With empty sets every old entry is dead by design; sweepLeakedChroots guards
// the /proc-unreadable case before it ever calls sweepDecision.
func TestSweepDecision_EmptyLiveAndActiveReturnsAllOldEntries(t *testing.T) {
	now := time.Now()
	active := func(string) bool { return false }
	live := map[string]bool{}
	entries := []string{"sb-dead-1", "sb-dead-2"}
	dead := sweepDecision(entries, oldModTimes(now, entries...), now, live, active)
	if len(dead) != len(entries) {
		t.Fatalf("expected all entries dead when live+active are both empty and entries are past the grace window, got %v", dead)
	}
}

// A fresh dir is the mid-boot window: the chroot exists but the process is not in /proc yet.
func TestIsSweepEligible_YoungDeadDirSkipped(t *testing.T) {
	now := time.Now()
	modTime := now.Add(-1 * time.Minute) // inside the grace window
	if isSweepEligible("sb-mid-boot", modTime, now, map[string]bool{}, false) {
		t.Fatalf("expected a dir younger than chrootSweepGrace to be skipped even though it is dead (not active, not live)")
	}
}

func TestIsSweepEligible_OldDeadDirSwept(t *testing.T) {
	now := time.Now()
	modTime := now.Add(-chrootSweepGrace - time.Minute) // past the grace window
	if !isSweepEligible("sb-long-dead", modTime, now, map[string]bool{}, false) {
		t.Fatalf("expected a dead dir older than chrootSweepGrace to be eligible for sweep")
	}
}

func TestIsSweepEligible_LiveOrActiveNeverEligible(t *testing.T) {
	now := time.Now()
	oldModTime := now.Add(-24 * time.Hour) // age alone clears the grace window

	if isSweepEligible("pool-deadbeef", oldModTime, now, map[string]bool{"pool-deadbeef": true}, false) {
		t.Fatalf("expected a live id to be skipped regardless of age")
	}
	if isSweepEligible("sb-abc123", oldModTime, now, map[string]bool{}, true) {
		t.Fatalf("expected an active id to be skipped regardless of age")
	}
}

// A /proc read failure must not become an empty live set that makes a live
// pool VM's chroot look dead.
func TestSweepLeakedChroots_BailsOnProcUnreadable(t *testing.T) {
	base := t.TempDir()
	oldBase := jailerBaseDir
	jailerBaseDir = base
	t.Cleanup(func() { jailerBaseDir = oldBase })

	oldLive := liveJailerIDs
	liveJailerIDs = func() (map[string]bool, bool) { return nil, false }
	t.Cleanup(func() { liveJailerIDs = oldLive })

	ids := []string{"sb-should-survive-0001", "pool-should-survive-cafe"}
	for _, id := range ids {
		if err := os.MkdirAll(filepath.Join(jailerChrootDir(), id, "root"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	m := &Manager{active: map[string]*Sandbox{}, log: zap.NewNop()}
	m.sweepLeakedChroots()

	for _, id := range ids {
		if _, err := os.Stat(filepath.Join(jailerChrootDir(), id)); err != nil {
			t.Fatalf("expected %s to survive a proc-unreadable sweep untouched, stat err=%v", id, err)
		}
	}
}

func TestSweepLeakedChroots_SkipsFreshDir(t *testing.T) {
	base := t.TempDir()
	oldBase := jailerBaseDir
	jailerBaseDir = base
	t.Cleanup(func() { jailerBaseDir = oldBase })

	oldLive := liveJailerIDs
	liveJailerIDs = func() (map[string]bool, bool) { return map[string]bool{}, true }
	t.Cleanup(func() { liveJailerIDs = oldLive })

	id := "sb-mid-boot-0001"
	if err := os.MkdirAll(filepath.Join(jailerChrootDir(), id, "root"), 0o755); err != nil {
		t.Fatal(err)
	}
	// os.MkdirAll leaves the dir's mtime at "now", inside the grace window.

	m := &Manager{active: map[string]*Sandbox{}, log: zap.NewNop()}
	m.sweepLeakedChroots()

	if _, err := os.Stat(filepath.Join(jailerChrootDir(), id)); err != nil {
		t.Fatalf("expected fresh (mid-boot) dir to survive the sweep, stat err=%v", err)
	}
}

func TestSweepLeakedChroots_SweepsOldDeadDir(t *testing.T) {
	base := t.TempDir()
	oldBase := jailerBaseDir
	jailerBaseDir = base
	t.Cleanup(func() { jailerBaseDir = oldBase })

	oldLive := liveJailerIDs
	liveJailerIDs = func() (map[string]bool, bool) { return map[string]bool{}, true }
	t.Cleanup(func() { liveJailerIDs = oldLive })

	id := "sb-long-dead-0002"
	dir := filepath.Join(jailerChrootDir(), id, "root")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-chrootSweepGrace - time.Minute)
	if err := os.Chtimes(filepath.Join(jailerChrootDir(), id), old, old); err != nil {
		t.Fatal(err)
	}

	m := &Manager{active: map[string]*Sandbox{}, log: zap.NewNop()}
	m.sweepLeakedChroots()

	if _, err := os.Stat(filepath.Join(jailerChrootDir(), id)); !os.IsNotExist(err) {
		t.Fatalf("expected dir older than chrootSweepGrace to be swept, stat err=%v", err)
	}
}
