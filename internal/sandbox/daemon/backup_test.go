package daemon

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/process"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/runtimetest"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// countingBlobs is an in-memory BlobStore that records how many bytes were
// handed to Put. The count is the point: a sparse capture that uploads its
// logical size rather than its real one costs real money.
type countingBlobs struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    int
	written int64
	putErr  error
}

func newCountingBlobs() *countingBlobs {
	return &countingBlobs{objects: map[string][]byte{}}
}

func (c *countingBlobs) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if c.putErr != nil {
		return c.putErr
	}
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.objects[key] = body
	c.puts++
	c.written += int64(len(body))
	return nil
}

func (c *countingBlobs) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.objects[key]
	if !ok {
		return nil, fmt.Errorf("artifact not found: %s", key)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (c *countingBlobs) Delete(ctx context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.objects[key]; !ok {
		return fmt.Errorf("artifact not found: %s", key)
	}
	delete(c.objects, key)
	return nil
}

func (c *countingBlobs) Name() string { return "counting" }

func (c *countingBlobs) stats() (puts int, written int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.puts, c.written
}

func newBackupDaemon(t *testing.T) (*Daemon, *runtimetest.Fake, *store.Store, *countingBlobs) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	fake := runtimetest.NewFake()
	blobs := newCountingBlobs()
	d := New(Config{
		Runtime: fake, Backups: st, Blobs: blobs,
		SnapshotDir: filepath.Join(dir, "snap"),
		IdleTimeout: time.Minute, HardTTL: time.Hour,
	})
	return d, fake, st, blobs
}

// waitBackup polls until the backup leaves pending, or fails the test.
func waitBackup(t *testing.T, st *store.Store, id string) store.Backup {
	t.Helper()
	for range 400 {
		b, ok := st.BackupByID(t.Context(), "default", id)
		if ok && b.Status != store.BackupPending {
			return b
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("backup %s never left pending", id)
	return store.Backup{}
}

func createRunning(t *testing.T, d *Daemon) SandboxInfo {
	t.Helper()
	sb, err := d.Create(t.Context(), CreateSpec{Template: "python-3.11", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return sb
}

// writeIn and readIn go through the sandbox's filesystem, which is what a
// capture has to carry and a restore has to bring back.
func writeIn(t *testing.T, d *Daemon, id, path, body string) {
	t.Helper()
	fs, err := d.FS(id)
	if err != nil {
		t.Fatalf("fs: %v", err)
	}
	if err := fs.Write(t.Context(), path, []byte(body)); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readIn(t *testing.T, d *Daemon, id, path string) (string, error) {
	t.Helper()
	fs, err := d.FS(id)
	if err != nil {
		return "", err
	}
	b, err := fs.Read(t.Context(), path)
	return string(b), err
}

// The headline: a restore must bring back the data the backup was taken from,
// not merely return bytes. Write A, back up, overwrite with B, restore, read A.
func TestBackupRestoreReturnsTheCapturedData(t *testing.T) {
	d, _, st, _ := newBackupDaemon(t)
	ctx := t.Context()
	sb := createRunning(t, d)

	writeIn(t, d, sb.ID, "/work/data.txt", "A")
	row, err := d.Backup(ctx, sb.ID)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if row.Status != store.BackupPending {
		t.Fatalf("the capture returned %q; it must return before the upload finishes", row.Status)
	}
	// The machine is running again before Backup returns; that is the promise.
	if got, _ := d.Get(sb.ID); got.State != StateRunning {
		t.Fatalf("sandbox is %s after the capture, want running", got.State)
	}
	ready := waitBackup(t, st, row.ID)
	if ready.Status != store.BackupReady {
		t.Fatalf("backup = %+v, want ready", ready)
	}

	writeIn(t, d, sb.ID, "/work/data.txt", "B")
	writeIn(t, d, sb.ID, "/work/after.txt", "written after the backup")

	// Positive control: without confirm nothing may change.
	if _, err := d.RestoreBackup(ctx, "default", sb.ID, row.ID, false); !errors.Is(err, ErrRestoreNotConfirmed) {
		t.Fatalf("unconfirmed restore = %v, want ErrRestoreNotConfirmed", err)
	}
	if got, _ := readIn(t, d, sb.ID, "/work/data.txt"); got != "B" {
		t.Fatalf("an unconfirmed restore changed the disk: data.txt reads %q, want B", got)
	}

	if _, err := d.RestoreBackup(ctx, "default", sb.ID, row.ID, true); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, err := readIn(t, d, sb.ID, "/work/data.txt")
	if err != nil {
		t.Fatalf("read after restore: %v", err)
	}
	if got != "A" {
		t.Fatalf("data.txt reads %q after the restore, want A — the restore returned bytes "+
			"but not the data the backup was taken from", got)
	}
	if _, err := readIn(t, d, sb.ID, "/work/after.txt"); err == nil {
		t.Error("a file written after the backup survived the restore; the disk was not replaced")
	}
	// Same machine, not a new one.
	if after, ok := d.Get(sb.ID); !ok || after.ID != sb.ID || after.State != StateRunning {
		t.Fatalf("after restore the sandbox is %+v; the id must be unchanged and running", after)
	}
}

// A restored sandbox must still be backupable. Firecracker tracks dirty pages
// against the memory file inside the directory it was restored from, so a
// restore that cleans up after itself breaks the *next* capture — which the
// unit tests missed and a live round trip found.
func TestBackupWorksAfterARestore(t *testing.T) {
	d, _, st, _ := newBackupDaemon(t)
	ctx := t.Context()
	sb := createRunning(t, d)

	writeIn(t, d, sb.ID, "/work/data.txt", "A")
	first, err := d.Backup(ctx, sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	waitBackup(t, st, first.ID)

	if _, err := d.RestoreBackup(ctx, "default", sb.ID, first.ID, true); err != nil {
		t.Fatalf("restore: %v", err)
	}
	// The directory the machine was restored from must still be there.
	restored := filepath.Join(d.checkpointRoot(sb.ID), restoreCheckpointName(first.ID))
	if _, err := os.Stat(restored); err != nil {
		t.Fatalf("the restore removed the directory the machine now depends on: %v", err)
	}
	// And it is not a checkpoint the owner took.
	list, _ := d.ListCheckpoints(sb.ID)
	for _, c := range list {
		if strings.HasPrefix(c.Name, restoreDirPrefix) {
			t.Errorf("a backup's materialisation is listed as a checkpoint: %+v", c)
		}
	}

	writeIn(t, d, sb.ID, "/work/data.txt", "C")
	second, err := d.Backup(ctx, sb.ID)
	if err != nil {
		t.Fatalf("capture after a restore: %v", err)
	}
	if b := waitBackup(t, st, second.ID); b.Status != store.BackupReady {
		t.Fatalf("the capture after a restore = %+v, want ready", b)
	}
	// Restoring that one gets C back, so the chain really works.
	writeIn(t, d, sb.ID, "/work/data.txt", "D")
	if _, err := d.RestoreBackup(ctx, "default", sb.ID, second.ID, true); err != nil {
		t.Fatalf("second restore: %v", err)
	}
	if got, _ := readIn(t, d, sb.ID, "/work/data.txt"); got != "C" {
		t.Fatalf("data.txt = %q after restoring the second backup, want C", got)
	}
	// Only the live materialisation survives; the earlier one is swept.
	old := filepath.Join(d.checkpointRoot(sb.ID), restoreCheckpointName(first.ID))
	if _, err := os.Stat(old); err == nil {
		t.Error("an earlier restore's directory is still on disk; they accumulate per restore")
	}
}

// A restore that fails must not take the running machine's memory base with
// it. The machine is still living off the directory an EARLIER restore left,
// so sweeping that one before the new restore has actually replaced the
// machine breaks the survivor's next capture.
func TestAFailedRestoreLeavesTheRunningMachineCapturable(t *testing.T) {
	d, _, st, blobs := newBackupDaemon(t)
	ctx := t.Context()
	sb := createRunning(t, d)

	writeIn(t, d, sb.ID, "/work/data.txt", "A")
	first, err := d.Backup(ctx, sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	waitBackup(t, st, first.ID)
	if _, err := d.RestoreBackup(ctx, "default", sb.ID, first.ID, true); err != nil {
		t.Fatalf("first restore: %v", err)
	}
	live := filepath.Join(d.checkpointRoot(sb.ID), restoreCheckpointName(first.ID))

	// A second backup whose object is corrupt, so its restore fails after the
	// point where a premature sweep would already have run.
	writeIn(t, d, sb.ID, "/work/data.txt", "B")
	second, err := d.Backup(ctx, sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	ready := waitBackup(t, st, second.ID)
	blobs.mu.Lock()
	blobs.objects[ready.ObjectKey] = []byte("not a gzip stream at all")
	blobs.mu.Unlock()

	if _, err := d.RestoreBackup(ctx, "default", sb.ID, second.ID, true); err == nil {
		t.Fatal("restoring a corrupt backup reported success")
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("the failed restore removed the directory the live machine depends on: %v", err)
	}
	// And the machine is still capturable, which is the thing that actually breaks.
	third, err := d.Backup(ctx, sb.ID)
	if err != nil {
		t.Fatalf("capture after a failed restore: %v", err)
	}
	if b := waitBackup(t, st, third.ID); b.Status != store.BackupReady {
		t.Fatalf("capture after a failed restore = %+v, want ready", b)
	}
}

// A capture that fails must leave the machine running and the row honest.
func TestBackupFailureLeavesTheSandboxRunning(t *testing.T) {
	d, fake, st, blobs := newBackupDaemon(t)
	ctx := t.Context()
	sb := createRunning(t, d)

	fake.CheckpointErr = errors.New("simulated capture failure")
	if _, err := d.Backup(ctx, sb.ID); err == nil {
		t.Fatal("a failing capture reported success")
	}
	if got, _ := d.Get(sb.ID); got.State != StateRunning {
		t.Fatalf("sandbox is %s after a failed capture, want running", got.State)
	}
	rows, err := st.ListSandboxBackups(ctx, "default", sb.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %+v, %v; want the failed attempt recorded", rows, err)
	}
	if rows[0].Status != store.BackupFailed || rows[0].Error == "" {
		t.Fatalf("row = %+v; a failed capture must say so and say why", rows[0])
	}
	if puts, _ := blobs.stats(); puts != 0 {
		t.Errorf("a failed capture uploaded %d object(s)", puts)
	}

	// And the sandbox is still backupable once the engine recovers.
	fake.CheckpointErr = nil
	row, err := d.Backup(ctx, sb.ID)
	if err != nil {
		t.Fatalf("backup after a failure: %v", err)
	}
	if b := waitBackup(t, st, row.ID); b.Status != store.BackupReady {
		t.Fatalf("second backup = %+v, want ready", b)
	}
}

// An upload failure must leave a failed row, not a pending one forever.
func TestBackupUploadFailureIsRecorded(t *testing.T) {
	d, _, st, blobs := newBackupDaemon(t)
	blobs.putErr = errors.New("object store is down")
	sb := createRunning(t, d)

	row, err := d.Backup(t.Context(), sb.ID)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	b := waitBackup(t, st, row.ID)
	if b.Status != store.BackupFailed || b.Error == "" {
		t.Fatalf("row = %+v; a failed upload must not leave the row pending", b)
	}
}

// An unchanged capture is a pointer at the bytes already stored, not a copy.
func TestBackupDedupesAnUnchangedCapture(t *testing.T) {
	d, _, st, blobs := newBackupDaemon(t)
	ctx := t.Context()
	sb := createRunning(t, d)
	writeIn(t, d, sb.ID, "/work/data.txt", "A")

	first, err := d.Backup(ctx, sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	firstReady := waitBackup(t, st, first.ID)
	putsAfterFirst, bytesAfterFirst := blobs.stats()
	if putsAfterFirst != 1 {
		t.Fatalf("the first capture made %d put(s), want 1", putsAfterFirst)
	}

	// Nothing written in between, so the capture is byte-identical.
	second, err := d.Backup(ctx, sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondReady := waitBackup(t, st, second.ID)

	puts, written := blobs.stats()
	if puts != putsAfterFirst || written != bytesAfterFirst {
		t.Errorf("an unchanged capture uploaded again: %d puts / %d bytes, was %d / %d",
			puts, written, putsAfterFirst, bytesAfterFirst)
	}
	if secondReady.ParentID != firstReady.ID {
		t.Fatalf("second backup's parent_id = %q, want %s", secondReady.ParentID, firstReady.ID)
	}
	if secondReady.ObjectKey != "" {
		t.Error("a deduped backup must not claim an object of its own")
	}

	// And it restores — from the parent's object.
	writeIn(t, d, sb.ID, "/work/data.txt", "B")
	if _, err := d.RestoreBackup(ctx, "default", sb.ID, secondReady.ID, true); err != nil {
		t.Fatalf("restore from a deduped backup: %v", err)
	}
	if got, _ := readIn(t, d, sb.ID, "/work/data.txt"); got != "A" {
		t.Fatalf("restore from the pointer row read %q, want A", got)
	}

	// A changed disk must NOT dedupe — otherwise the test above passes because
	// dedupe always fires, which would be a data-loss bug, not a saving.
	writeIn(t, d, sb.ID, "/work/data.txt", "C")
	third, err := d.Backup(ctx, sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	thirdReady := waitBackup(t, st, third.ID)
	if thirdReady.ParentID != "" || thirdReady.ObjectKey == "" {
		t.Fatalf("a changed disk deduped: %+v", thirdReady)
	}
	if p, _ := blobs.stats(); p != 2 {
		t.Errorf("puts = %d, want 2 (the first capture and the changed one)", p)
	}
}

// A dedupe target that owns no object and names no parent cannot be restored
// from. Pointing at it would make the new row unrestorable too, so the capture
// uploads instead — a smaller row is not worth a backup that is not one.
func TestDedupeUploadsRatherThanPointAtAnUnrestorableRow(t *testing.T) {
	d, _, st, blobs := newBackupDaemon(t)
	ctx := t.Context()
	sb := createRunning(t, d)

	// Learn the digest this sandbox's capture produces.
	writeIn(t, d, sb.ID, "/work/data.txt", "A")
	probe, err := d.Backup(ctx, sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	digest := waitBackup(t, st, probe.ID).SHA256
	if digest == "" {
		t.Fatal("no digest to work with")
	}

	// Now the newest ready row claims that digest but holds neither an object
	// nor a parent: what a half-finished write or a hand-edited row looks like.
	orphan, err := st.CreateBackup(ctx, store.Backup{OrgID: "default", SandboxID: sb.ID})
	if err != nil {
		t.Fatal(err)
	}
	// ready_at has one-second granularity and the tiebreak below it is the
	// random id, so the orphan is marked ready a whole second later rather than
	// left to a coin flip. Without this the test passes about half the time,
	// which is worse than not having it.
	time.Sleep(1100 * time.Millisecond)
	if err := st.SetBackupReady(ctx, orphan.ID, "memory", "", digest, "", 1); err != nil {
		t.Fatal(err)
	}
	if last, _ := st.LastReadyBackup(ctx, sb.ID); last.ID != orphan.ID {
		t.Fatalf("the orphan is not the dedupe target (%s is); the test would prove nothing", last.ID)
	}

	putsBefore, _ := blobs.stats()
	next, err := d.Backup(ctx, sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := waitBackup(t, st, next.ID)
	if got.ParentID != "" {
		t.Errorf("the capture pointed at an unrestorable row (%s); it must upload instead", got.ParentID)
	}
	if got.ObjectKey == "" {
		t.Error("the capture has neither a parent nor an object, so it cannot be restored")
	}
	if puts, _ := blobs.stats(); puts != putsBefore+1 {
		t.Errorf("puts went %d → %d; want exactly one upload", putsBefore, puts)
	}
}

// A second capture while one is in flight is refused, not queued behind a
// gigabyte upload.
func TestBackupRefusesASecondCaptureInFlight(t *testing.T) {
	d, _, st, blobs := newBackupDaemon(t)
	ctx := t.Context()
	sb := createRunning(t, d)

	// Hold the upload open so the first row stays pending.
	release := make(chan struct{})
	blobs.putErr = nil
	slow := &blockingBlobs{inner: blobs, release: release}
	d.cfg.Blobs = slow

	first, err := d.Backup(ctx, sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	waitPending(t, st, first.ID)

	if _, err := d.Backup(ctx, sb.ID); !errors.Is(err, ErrBackupInFlight) {
		t.Fatalf("second capture = %v, want ErrBackupInFlight", err)
	}
	// A restore while a capture is reading the machine is refused too.
	if _, err := d.RestoreBackup(ctx, "default", sb.ID, first.ID, true); err == nil {
		t.Error("a restore ran while a capture was in flight")
	}

	close(release)
	if b := waitBackup(t, st, first.ID); b.Status != store.BackupReady {
		t.Fatalf("first backup = %+v after release, want ready", b)
	}
	// And now a second one is allowed.
	if _, err := d.Backup(ctx, sb.ID); err != nil {
		t.Fatalf("capture after the first finished: %v", err)
	}
}

// Delete guards: a pending capture and a referenced row both refuse.
func TestDeleteBackupGuards(t *testing.T) {
	d, _, st, blobs := newBackupDaemon(t)
	ctx := t.Context()
	sb := createRunning(t, d)

	first, _ := d.Backup(ctx, sb.ID)
	firstReady := waitBackup(t, st, first.ID)
	second, _ := d.Backup(ctx, sb.ID)
	secondReady := waitBackup(t, st, second.ID)
	if secondReady.ParentID != firstReady.ID {
		t.Fatalf("expected the second capture to dedupe; got %+v", secondReady)
	}

	if err := d.DeleteBackup(ctx, "default", sb.ID, firstReady.ID); !errors.Is(err, store.ErrBackupReferenced) {
		t.Fatalf("deleting a referenced backup = %v, want ErrBackupReferenced", err)
	}
	if _, ok := blobs.objects[firstReady.ObjectKey]; !ok {
		t.Error("the refused delete removed the object anyway")
	}

	// The leaf first, then the object goes with the row it belonged to.
	if err := d.DeleteBackup(ctx, "default", sb.ID, secondReady.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteBackup(ctx, "default", sb.ID, firstReady.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := blobs.objects[firstReady.ObjectKey]; ok {
		t.Error("the row went but its object is still in the store, where nothing points at it")
	}
	if rows, _ := st.ListSandboxBackups(ctx, "default", sb.ID); len(rows) != 0 {
		t.Fatalf("rows survived: %+v", rows)
	}
}

// A runtime that cannot capture, and a node with no object store, both refuse
// clearly rather than failing somewhere deeper.
func TestBackupRefusalsNameWhatIsMissing(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	// No object store.
	noBlobs := New(Config{
		Runtime: runtimetest.NewFake(), Backups: st,
		SnapshotDir: filepath.Join(dir, "a"), IdleTimeout: time.Minute, HardTTL: time.Hour,
	})
	sb := createRunning(t, noBlobs)
	_, err = noBlobs.Backup(t.Context(), sb.ID)
	if !errors.Is(err, ErrBackupsUnconfigured) {
		t.Fatalf("backup with no object store = %v, want ErrBackupsUnconfigured", err)
	}

	// A runtime with no Checkpointer.
	noCapture := New(Config{
		Runtime: process.New(filepath.Join(dir, "p")), Backups: st, Blobs: newCountingBlobs(),
		SnapshotDir: filepath.Join(dir, "b"), IdleTimeout: time.Minute, HardTTL: time.Hour,
	})
	sb2 := createRunning(t, noCapture)
	_, err = noCapture.Backup(t.Context(), sb2.ID)
	if !errors.Is(err, rt.ErrUnsupported) {
		t.Fatalf("backup on a runtime that cannot capture = %v, want rt.ErrUnsupported", err)
	}
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte(noCapture.RuntimeName())) {
		t.Errorf("the refusal must name the runtime, got %v", err)
	}
}

// adoptAll brings every persisted sandbox back after a restart, handing back
// the fake's own instance so the capture path still finds a checkpointer.
type adoptAll struct{ *runtimetest.Fake }

func (a adoptAll) Adopt(ctx context.Context, id string) (rt.Instance, bool) {
	inst, err := a.Boot(ctx, rt.BootSpec{SandboxID: id})
	return inst, err == nil
}

// Retention used to live only in a struct field in this process: the backup
// cron job carries no retention column, so a node restart silently put every
// sandbox back on BACKUPS_RETAIN_DAYS. The test is the restart — a second
// daemon over the same database — and the assertion is the expiry actually
// stamped on a capture taken after it, not the field.
func TestBackupRetentionSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	fake := adoptAll{Fake: runtimetest.NewFake()}
	blobs := newCountingBlobs()
	cfg := func() Config {
		return Config{
			Runtime: fake, Store: StoreAdapter{S: st}, Backups: st, Blobs: blobs,
			SnapshotDir: filepath.Join(dir, "snap"),
			IdleTimeout: time.Hour, HardTTL: time.Hour,
		}
	}

	const retain = 48 * time.Hour
	d := New(cfg())
	sb := createRunning(t, d)
	if err := d.SetBackupRetention(t.Context(), sb.ID, retain); err != nil {
		t.Fatalf("set retention: %v", err)
	}

	// The restart. Nothing is carried over but the database.
	d2 := New(cfg())
	d2.Start()
	t.Cleanup(d2.Stop)

	if got := d2.BackupRetention(sb.ID); got != retain {
		t.Fatalf("retention after a restart = %s, want %s", got, retain)
	}
	before := time.Now()
	row, err := d2.Backup(t.Context(), sb.ID)
	if err != nil {
		t.Fatalf("backup after restart: %v", err)
	}
	want := before.Add(retain).Unix()
	if row.ExpiresAt < want-60 || row.ExpiresAt > want+60 {
		t.Fatalf("a backup taken after the restart expires at %d, want about %d "+
			"(the sandbox's own %s, not the deployment default)", row.ExpiresAt, want, retain)
	}
}

// A mostly-empty disk must not be uploaded as its logical size. 1 GiB holding
// 4 MiB of data has to cross the wire as single-digit megabytes.
func TestPackCaptureDoesNotSendHoles(t *testing.T) {
	dir := t.TempDir()
	const logical = 1 << 30
	const data = 4 << 20

	f, err := os.Create(filepath.Join(dir, "disk.ext4"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(logical); err != nil {
		t.Fatal(err)
	}
	// Real data in the middle, so the hole is on both sides of it.
	payload := bytes.Repeat([]byte("pilldrops"), data/9)
	if _, err := f.WriteAt(payload, logical/2); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	packed, digest, size, err := packCapture(dir)
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	t.Cleanup(func() { packed.Close(); os.Remove(packed.Name()) })

	if size >= 8<<20 {
		t.Fatalf("a 1 GiB disk holding %d bytes packed to %d bytes; the holes are being sent", len(payload), size)
	}
	if digest == "" {
		t.Fatal("no digest, so nothing can dedupe")
	}

	// And it round-trips: the hole comes back as zeros and the data as data.
	out := filepath.Join(dir, "restored")
	if err := unpackCapture(packed, out); err != nil {
		t.Fatalf("unpack: %v", err)
	}
	got, err := os.Open(filepath.Join(out, "disk.ext4"))
	if err != nil {
		t.Fatal(err)
	}
	defer got.Close()
	info, _ := got.Stat()
	if info.Size() != logical {
		t.Fatalf("restored size = %d, want %d", info.Size(), logical)
	}
	back := make([]byte, len(payload))
	if _, err := got.ReadAt(back, logical/2); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back, payload) {
		t.Fatal("the data did not survive the round trip")
	}
	hole := make([]byte, 4096)
	if _, err := got.ReadAt(hole, 1024); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(hole, make([]byte, 4096)) {
		t.Fatal("a hole came back holding something other than zeros")
	}
}

// A backup arrives from a store other nodes also write to, so the archive is
// not trusted: an entry name that escapes the directory must be refused, not
// sanitised into something that lands anyway.
func TestUnpackRefusesAnEscapingEntry(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"../escape", "../../etc/cron.d/evil", "sub/dir", `..\windows`, "..", ".", "",
	} {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(zw)
		body := []byte("owned")
		_ = tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg, Name: name, Size: int64(len(body)), Mode: 0o600,
		})
		_, _ = tw.Write(body)
		_ = tw.Close()
		_ = zw.Close()

		root := t.TempDir()
		dir := filepath.Join(root, "capture")
		err := unpackCapture(&buf, dir)
		if err == nil {
			t.Errorf("unpack accepted the entry name %q", name)
		}
		// And nothing landed outside the capture directory either way.
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, werr error) error {
			if werr != nil || d.IsDir() {
				return nil //nolint:nilerr // a walk error is not what this test is about
			}
			if !strings.HasPrefix(p, dir+string(os.PathSeparator)) {
				t.Errorf("entry %q wrote %s, outside the capture directory", name, p)
			}
			return nil
		})
	}
}

// Two captures of identical bytes must hash the same, or dedupe never fires.
func TestPackCaptureDigestIsContentOnly(t *testing.T) {
	mk := func(t *testing.T, body string) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "disk"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		// A different mtime on the second capture is the realistic case.
		time.Sleep(2 * time.Millisecond)
		f, digest, _, err := packCapture(dir)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
		os.Remove(f.Name())
		return digest
	}
	if a, b := mk(t, "same"), mk(t, "same"); a != b {
		t.Fatalf("identical captures hashed differently (%s vs %s); nothing would ever dedupe", a, b)
	}
	if a, b := mk(t, "same"), mk(t, "different"); a == b {
		t.Fatal("different captures hashed the same; a changed disk would be silently discarded")
	}
}

// blockingBlobs holds a Put open until release is closed.
type blockingBlobs struct {
	inner   *countingBlobs
	release chan struct{}
}

func (b *blockingBlobs) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	<-b.release
	return b.inner.Put(ctx, key, r, size)
}
func (b *blockingBlobs) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return b.inner.Get(ctx, key)
}
func (b *blockingBlobs) Delete(ctx context.Context, key string) error {
	return b.inner.Delete(ctx, key)
}
func (b *blockingBlobs) Name() string { return "blocking" }

func waitPending(t *testing.T, st *store.Store, id string) {
	t.Helper()
	for range 200 {
		if b, ok := st.BackupByID(t.Context(), "default", id); ok && b.Status == store.BackupPending {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the backup never reached pending")
}
