package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/runtimetest"
)

// newCheckpointDaemon gives the daemon a SnapshotDir of its own, since
// checkpoints are directories under it.
func newCheckpointDaemon(t *testing.T) (*Daemon, *runtimetest.Fake) {
	t.Helper()
	fake := runtimetest.NewFake()
	d := New(Config{
		Runtime:     fake,
		IdleTimeout: time.Minute,
		HardTTL:     time.Hour,
		SnapshotDir: t.TempDir(),
	})
	return d, fake
}

func TestCheckpointSaveListRestoreRemove(t *testing.T) {
	d, fake := newCheckpointDaemon(t)
	ctx := context.Background()
	sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := d.StartServer(ctx, sb.ID, "serve.sh", 8000, nil); err != nil {
		t.Fatalf("start server: %v", err)
	}

	c, err := d.Checkpoint(ctx, sb.ID, "good")
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if c.Kind != "memory" || c.Status != "ready" {
		t.Fatalf("checkpoint = %+v, want a ready memory checkpoint", c)
	}
	dir := filepath.Join(d.checkpointRoot(sb.ID), "good")
	if _, err := os.Stat(filepath.Join(dir, "payload")); err != nil {
		t.Fatalf("the engine's payload is not in the published dir: %v", err)
	}

	list, err := d.ListCheckpoints(sb.ID)
	if err != nil || len(list) != 1 || list[0].Name != "good" {
		t.Fatalf("list = %+v, %v; want one checkpoint named good", list, err)
	}

	// A memory checkpoint comes back with its processes, so the start command
	// must NOT be relaunched.
	serversBefore := len(fake.ServerLog(sb.ID))
	if _, err := d.RestoreCheckpoint(ctx, sb.ID, "good"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := fake.CheckpointRestores(); len(got) != 1 || got[0] != sb.ID+"/good" {
		t.Fatalf("engine restores = %v, want one for good", got)
	}
	if n := len(fake.ServerLog(sb.ID)); n != 0 {
		t.Errorf("a memory restore relaunched the start command %d time(s) on the new instance "+
			"(had %d before); the processes are already running", n, serversBefore)
	}
	if sb, _ := d.Get(sb.ID); sb.State != StateRunning {
		t.Errorf("state after restore = %s, want running", sb.State)
	}

	if err := d.RemoveCheckpoint(sb.ID, "good"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if list, _ := d.ListCheckpoints(sb.ID); len(list) != 0 {
		t.Fatalf("list after remove = %+v, want empty", list)
	}
	if err := d.RemoveCheckpoint(sb.ID, "good"); !errors.Is(err, rt.ErrNotFound) {
		t.Errorf("removing a checkpoint that is not there = %v, want ErrNotFound", err)
	}
}

// A disk checkpoint has lost the guest's processes, so the start command is relaunched.
func TestDiskCheckpointRestoreRelaunchesTheStartCommand(t *testing.T) {
	d, fake := newCheckpointDaemon(t)
	fake.CheckpointKind = "disk"
	ctx := context.Background()
	sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := d.StartServer(ctx, sb.ID, "serve.sh", 8000, nil); err != nil {
		t.Fatalf("start server: %v", err)
	}
	if _, err := d.Checkpoint(ctx, sb.ID, "d1"); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if _, err := d.RestoreCheckpoint(ctx, sb.ID, "d1"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := fake.ServerLog(sb.ID); len(got) != 1 || got[0] != "serve.sh" {
		t.Fatalf("start commands after a disk restore = %v, want [serve.sh]", got)
	}
}

func TestCheckpointCapAndNames(t *testing.T) {
	d, _ := newCheckpointDaemon(t)
	ctx := context.Background()
	sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 0; i < MaxCheckpoints; i++ {
		if _, err := d.Checkpoint(ctx, sb.ID, string(rune('a'+i))); err != nil {
			t.Fatalf("checkpoint %d: %v", i, err)
		}
	}
	if _, err := d.Checkpoint(ctx, sb.ID, "onetoomany"); !errors.Is(err, ErrCheckpointLimit) {
		t.Fatalf("the %dth save = %v, want ErrCheckpointLimit", MaxCheckpoints+1, err)
	}
	// Replacing one of the ten is not a new one and must be allowed.
	if _, err := d.Checkpoint(ctx, sb.ID, "a"); err != nil {
		t.Fatalf("re-saving over an existing name at the cap: %v", err)
	}
	if list, _ := d.ListCheckpoints(sb.ID); len(list) != MaxCheckpoints {
		t.Fatalf("re-save changed the count to %d, want %d", len(list), MaxCheckpoints)
	}
	for _, bad := range []string{"", "Upper", "-lead", "has space", "way-too-long-a-name-for-a-checkpoint-directory-x"} {
		if _, err := d.Checkpoint(ctx, sb.ID, bad); err == nil {
			t.Errorf("name %q was accepted", bad)
		}
	}
}

// A capture that never finished leaves a directory with no checkpoint.json. It
// must be visible (it holds disk) and refused by a restore.
func TestUnfinishedCheckpointIsBuildingAndNotRestorable(t *testing.T) {
	d, _ := newCheckpointDaemon(t)
	ctx := context.Background()
	sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(d.checkpointRoot(sb.ID), "half"), 0o755); err != nil {
		t.Fatal(err)
	}
	list, _ := d.ListCheckpoints(sb.ID)
	if len(list) != 1 || list[0].Status != "building" {
		t.Fatalf("list = %+v, want one building checkpoint", list)
	}
	if _, err := d.RestoreCheckpoint(ctx, sb.ID, "half"); !errors.Is(err, ErrCheckpointNotReady) {
		t.Fatalf("restoring a building checkpoint = %v, want ErrCheckpointNotReady", err)
	}
}

// A failed capture must not destroy the checkpoint it was replacing.
func TestFailedSaveLeavesTheOldCheckpointWhole(t *testing.T) {
	d, fake := newCheckpointDaemon(t)
	ctx := context.Background()
	sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := d.Checkpoint(ctx, sb.ID, "keep"); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	want, err := os.ReadFile(filepath.Join(d.checkpointRoot(sb.ID), "keep", "payload"))
	if err != nil {
		t.Fatal(err)
	}

	fake.CheckpointErr = errors.New("engine exploded")
	if _, err := d.Checkpoint(ctx, sb.ID, "keep"); err == nil {
		t.Fatal("the failing save reported success")
	}
	got, err := os.ReadFile(filepath.Join(d.checkpointRoot(sb.ID), "keep", "payload"))
	if err != nil || string(got) != string(want) {
		t.Fatalf("the old checkpoint did not survive a failed re-save: %q, %v", got, err)
	}
	if list, _ := d.ListCheckpoints(sb.ID); len(list) != 1 {
		t.Fatalf("list = %+v, want the one surviving checkpoint (no .tmp leftovers)", list)
	}
}

// Checkpoints die with the sandbox, and a stray directory is swept at start.
func TestDestroyAndSweepRemoveCheckpointDirs(t *testing.T) {
	d, _ := newCheckpointDaemon(t)
	ctx := context.Background()
	sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := d.Checkpoint(ctx, sb.ID, "c1"); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	root := d.checkpointRoot(sb.ID)
	if err := d.Destroy(ctx, sb.ID); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("checkpoint dir survived destroy: %v", err)
	}

	stray := d.checkpointRoot("sb-never-existed")
	if err := os.MkdirAll(filepath.Join(stray, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	d.sweepCheckpoints()
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatalf("stray checkpoint dir survived the sweep: %v", err)
	}
}

// A runtime with no Checkpointer answers ErrUnsupported, which the API renders 501.
func TestCheckpointUnsupportedRuntime(t *testing.T) {
	d := New(Config{Runtime: noCheckpointRuntime{runtimetest.NewFake()}, SnapshotDir: t.TempDir(), HardTTL: time.Hour})
	ctx := context.Background()
	sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := d.Checkpoint(ctx, sb.ID, "c1"); !errors.Is(err, rt.ErrUnsupported) {
		t.Fatalf("checkpoint on a runtime that cannot = %v, want ErrUnsupported", err)
	}
}

// noCheckpointRuntime hides the Fake's Checkpointer methods behind the bare
// Runtime interface.
type noCheckpointRuntime struct{ inner rt.Runtime }

func (n noCheckpointRuntime) Name() string          { return n.inner.Name() }
func (n noCheckpointRuntime) Capabilities() rt.Caps { return n.inner.Capabilities() }
func (n noCheckpointRuntime) Prepare(ctx context.Context, t rt.Template) error {
	return n.inner.Prepare(ctx, t)
}
func (n noCheckpointRuntime) Boot(ctx context.Context, s rt.BootSpec) (rt.Instance, error) {
	return n.inner.Boot(ctx, s)
}
func (n noCheckpointRuntime) Restore(ctx context.Context, snap rt.SnapshotRef, s rt.BootSpec) (rt.Instance, error) {
	return n.inner.Restore(ctx, snap, s)
}
func (n noCheckpointRuntime) RecoverOrphans() []rt.Instance { return n.inner.RecoverOrphans() }

// A named-snapshot save and a backup ride an ordinary capture directory beside
// the owner's save points. They are not save points anyone took: a sandbox
// holding ten checkpoints could never be snapshotted again, and the refusal
// arrived after the 202 had gone out, as `status: failed`.
func TestPlatformCapturesAreNotTheOwnersCheckpoints(t *testing.T) {
	d, _ := newCheckpointDaemon(t)
	ctx := context.Background()
	sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 0; i < MaxCheckpoints; i++ {
		if _, err := d.Checkpoint(ctx, sb.ID, string(rune('a'+i))); err != nil {
			t.Fatalf("checkpoint %d: %v", i, err)
		}
	}

	// The sandbox is at the cap, which is a fact about the owner's save points.
	export := ExportDirPrefix + "bld-0123456789ab"
	if _, err := d.Checkpoint(ctx, sb.ID, export); err != nil {
		t.Fatalf("snapshot export at the checkpoint cap: %v — a sandbox holding "+
			"%d checkpoints must still be snapshottable", err, MaxCheckpoints)
	}
	if _, err := d.Checkpoint(ctx, sb.ID, backupCheckpointName("bkp_1")); err != nil {
		t.Fatalf("backup capture at the checkpoint cap: %v", err)
	}

	// And neither one shows up as a checkpoint or eats a slot.
	list, err := d.ListCheckpoints(sb.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != MaxCheckpoints {
		t.Fatalf("list has %d entries, want the owner's %d: %+v", len(list), MaxCheckpoints, list)
	}
	for _, c := range list {
		if InternalCapture(c.Name) {
			t.Errorf("%q is the platform's capture and is listed as a checkpoint", c.Name)
		}
	}
	// The cap itself still holds for the owner.
	if _, err := d.Checkpoint(ctx, sb.ID, "onetoomany"); !errors.Is(err, ErrCheckpointLimit) {
		t.Fatalf("the %dth owner save = %v, want ErrCheckpointLimit", MaxCheckpoints+1, err)
	}
}

// A caller's own image on a plain template is in no catalogue: only the record
// can say what to boot again.
func TestRestoresKeepACallerSuppliedImageAndPorts(t *testing.T) {
	d, fake := newCheckpointDaemon(t)
	ctx := context.Background()
	sb, err := d.Create(ctx, CreateSpec{Template: "base", Image: "example/app:1", Ports: []int{8080}, OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := d.Pause(ctx, sb.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := d.Resume(ctx, sb.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	spec, _ := fake.LastBootSpec()
	if spec.Template.Image != "example/app:1" || len(spec.Template.Ports) != 1 || spec.Template.Ports[0] != 8080 {
		t.Errorf("resume booted with image=%q ports=%v, want example/app:1 [8080]", spec.Template.Image, spec.Template.Ports)
	}
	d.mu.Lock()
	rec := d.recordOf(d.boxes[sb.ID])
	d.mu.Unlock()
	if rec.Image != "example/app:1" || len(rec.Ports) != 1 || rec.Ports[0] != 8080 {
		t.Errorf("record keeps image=%q ports=%v, want example/app:1 [8080]", rec.Image, rec.Ports)
	}
}
