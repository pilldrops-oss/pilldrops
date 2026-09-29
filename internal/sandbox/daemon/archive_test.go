package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/runtime/process"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/runtimetest"
)

func TestDirArchiveRoundTripFile(t *testing.T) {
	a, err := NewDirArchive(t.TempDir())
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	src := filepath.Join(t.TempDir(), "sb.snap")
	if err := os.WriteFile(src, []byte("snapshot bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Put(context.Background(), "sb1", src); err != nil {
		t.Fatalf("put: %v", err)
	}

	dst := filepath.Join(t.TempDir(), "restored.snap")
	ok, err := a.Get(context.Background(), "sb1", dst)
	if err != nil || !ok {
		t.Fatalf("get = %v, %v; want true, nil", ok, err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "snapshot bytes" {
		t.Errorf("restored %q, want %q", got, "snapshot bytes")
	}

	if err := a.Delete(context.Background(), "sb1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if ok, _ := a.Get(context.Background(), "sb1", dst+"2"); ok {
		t.Error("snapshot still present after delete — the archive would grow forever")
	}
}

// Firecracker snapshots are directories, process snapshots a single tar; both shapes must survive.
func TestDirArchiveRoundTripDirectory(t *testing.T) {
	a, err := NewDirArchive(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "snapdir")
	if err := os.MkdirAll(filepath.Join(src, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "state"), []byte("vm state"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "nested", "mem"), []byte("guest memory"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := a.Put(context.Background(), "fc1", src); err != nil {
		t.Fatalf("put: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "out")
	if ok, err := a.Get(context.Background(), "fc1", dst); err != nil || !ok {
		t.Fatalf("get = %v, %v", ok, err)
	}
	for rel, want := range map[string]string{
		"state":      "vm state",
		"nested/mem": "guest memory",
	} {
		got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
}

// A miss is (false, nil), not an error; restore tells the two apart.
func TestDirArchiveGetMissing(t *testing.T) {
	a, _ := NewDirArchive(t.TempDir())
	ok, err := a.Get(context.Background(), "nope", filepath.Join(t.TempDir(), "x"))
	if ok || err != nil {
		t.Fatalf("get missing = %v, %v; want false, nil", ok, err)
	}
}

func TestSnapshotRecoveredAfterNodeDiskLoss(t *testing.T) {
	clk := &manualClock{t: time.Unix(1_000_000, 0)}
	// The real process runtime: the fake's Snapshot writes nothing to archive.
	rtp := process.New(filepath.Join(t.TempDir(), "sb"))
	fp := newFakePersister(nil)
	archive, err := NewDirArchive(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	snapDir := t.TempDir()

	d := New(Config{
		Runtime: rtp, Store: fp, Clock: clk,
		SnapshotDir: snapDir, SnapshotArchive: archive,
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	sb, _ := d.Create(context.Background(), CreateSpec{Template: "t", Timeout: -1})
	if err := d.Pause(context.Background(), sb.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}

	fp.mu.Lock()
	rec := fp.saved[sb.ID]
	fp.mu.Unlock()
	if rec.Snapshot == "" {
		t.Fatal("paused record has no snapshot path")
	}

	// Simulate node loss: local snapshot disk gone, store record survives.
	if err := os.RemoveAll(snapDir); err != nil {
		t.Fatal(err)
	}

	d2 := New(Config{
		Runtime: rtp, Store: newFakePersister([]PersistedSandbox{rec}), Clock: clk,
		SnapshotDir: snapDir, SnapshotArchive: archive,
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	d2.Start()
	defer d2.Stop()

	got, ok := d2.Get(sb.ID)
	if !ok {
		t.Fatal("workstation dropped after node disk loss — the archive was not consulted")
	}
	if got.State != StatePaused {
		t.Fatalf("state = %s, want paused", got.State)
	}
	if _, err := os.Stat(rec.Snapshot); err != nil {
		t.Fatalf("snapshot not restored to local disk: %v", err)
	}
	if err := d2.Resume(context.Background(), sb.ID); err != nil {
		t.Fatalf("resume after recovery: %v", err)
	}
}

// A missing snapshot file must never delete the record: a slow network mount at
// boot looks identical, and dropping on it would wipe the fleet.
func TestMissingSnapshotIsNotDropped(t *testing.T) {
	clk := &manualClock{t: time.Unix(1_000_000, 0)}
	fake := runtimetest.NewFake()
	archive, _ := NewDirArchive(t.TempDir()) // configured, but empty
	rec := PersistedSandbox{
		ID: "sb_gone", Template: "t", OrgID: "default", State: "paused",
		Snapshot:  filepath.Join(t.TempDir(), "vanished.snap"),
		CreatedAt: clk.Now().Unix() - 100, ExpiresAt: 0,
	}
	d := New(Config{
		Runtime: fake, Store: newFakePersister([]PersistedSandbox{rec}), Clock: clk,
		SnapshotDir: t.TempDir(), SnapshotArchive: archive,
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	d.Start()
	defer d.Stop()

	if _, ok := d.Get("sb_gone"); !ok {
		t.Error("record deleted because a snapshot file was absent — that is unrecoverable data loss on a transient mount failure")
	}
}
