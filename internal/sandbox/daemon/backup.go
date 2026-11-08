package daemon

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/pilldrops-oss/pilldrops/internal/config"
	"github.com/pilldrops-oss/pilldrops/internal/observability"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// Backups: a capture of a sandbox, sent off the node to object storage, and
// restorable back onto the same sandbox id.
//
// The capture is the checkpoint primitive, not a second one. What a backup
// adds is that the bytes leave the machine: a checkpoint dies with its node,
// which is the whole failure this exists for.

var (
	// ErrBackupsUnconfigured means this node has no object store to put a
	// backup in. The control-plane artifact store does not count: it is
	// Postgres or a directory behind the orchestrator and was never meant to
	// hold gigabyte disk images.
	ErrBackupsUnconfigured = errors.New("backups_need_object_store")
	// ErrBackupInFlight means a capture of this sandbox is already running.
	ErrBackupInFlight = errors.New("a backup of this sandbox is already running")
	// ErrBackupNotReady means the capture never finished, so there is nothing
	// to restore from.
	ErrBackupNotReady = errors.New(`backup is not ready: poll GET /v1/sandboxes/{id}/backups until its status is "ready"`)
	// ErrRestoreNotConfirmed means the caller did not say yes to losing the
	// sandbox's current disk.
	ErrRestoreNotConfirmed = errors.New("restore overwrites the sandbox's current disk and needs confirm: true")
)

// BackupStore is the row index backups need. *store.Store satisfies it.
type BackupStore interface {
	CreateBackup(ctx context.Context, b store.Backup) (store.Backup, error)
	SetBackupReady(ctx context.Context, id, kind, objectKey, sha256hex, parentID string, bytes int64) error
	SetBackupFailed(ctx context.Context, id, reason string) error
	BackupByID(ctx context.Context, orgID, id string) (store.Backup, bool)
	ListSandboxBackups(ctx context.Context, orgID, sandboxID string) ([]store.Backup, error)
	LastReadyBackup(ctx context.Context, sandboxID string) (store.Backup, bool)
	PendingBackup(ctx context.Context, sandboxID string) (store.Backup, bool)
	BackupChildren(ctx context.Context, id string) ([]string, error)
	ExpiredBackups(ctx context.Context, now int64, defaultDays, limit int) ([]store.Backup, error)
	DeleteBackup(ctx context.Context, orgID, id string) error
}

// BlobStore is the object storage a backup is written to: four verbs of
// artifact.StorageProvider, named here rather than imported so the daemon does
// not link the storage plane, the same reason Config.PinTemplate is a func.
type BlobStore interface {
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
	Name() string
}

// backupPruneInterval is how often expired backups are reclaimed. Hourly:
// retention bounds storage growth, it is not a deletion deadline.
const backupPruneInterval = time.Hour

// BackupRetainDays is the deployment default for how long a backup is kept
// when its sandbox sets no retention of its own. 0 keeps them forever.
func BackupRetainDays() int {
	return config.Int("BACKUPS_RETAIN_DAYS", 7)
}

// backupBlockSize is the granularity the restore writer tests for holes. 64 KiB
// is large enough that the zero test costs nothing next to the I/O and small
// enough to keep a mostly-empty disk sparse.
const backupBlockSize = 64 << 10

// BackupKey is where a backup's bytes live. Exported so a caller holding only
// the store can reclaim an object whose row it is deleting.
func BackupKey(orgID, sandboxID, backupID string) string {
	return "backups/" + orgID + "/" + sandboxID + "/" + backupID + ".tar.gz"
}

// backupCheckpointName is the capture directory a backup uses. It is not a
// user-visible checkpoint: it lives and dies inside one Backup call, and it is
// removed once the bytes are in the object store.
//
// The restore materialises into a *different* directory (prefix "rst-") even
// though it holds the same capture. Sharing the name is a live race: the
// upload's cleanup removes its directory after it marks the row ready, so a
// restore that started on the ready row would have the ground taken out from
// under it mid-write.
func backupCheckpointName(backupID string) string {
	return backupDirPrefix + strings.TrimPrefix(strings.ReplaceAll(backupID, "_", "-"), backupDirPrefix)
}

func restoreCheckpointName(backupID string) string {
	return restoreDirPrefix + strings.TrimPrefix(strings.ReplaceAll(backupID, "_", "-"), backupDirPrefix)
}

// restoreDirPrefix marks a directory a backup was materialised into. It is a
// capture directory, not a save point: ListCheckpoints hides it so it neither
// shows up as a checkpoint the owner did not take nor eats one of the ten
// slots MaxCheckpoints allows.
const restoreDirPrefix = "rst-"

// sweepRestoreDirs removes this sandbox's previous restore materialisations,
// keeping only `keep`. A restored VM's memory base lives in the directory it
// came from until its next snapshot, so exactly one has to survive — the live
// one — and the rest are dead weight.
func (d *Daemon) sweepRestoreDirs(id, keep string) {
	entries, err := os.ReadDir(d.checkpointRoot(id))
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), restoreDirPrefix) || e.Name() == keep {
			continue
		}
		_ = os.RemoveAll(filepath.Join(d.checkpointRoot(id), e.Name()))
	}
}

// BackupsReady reports whether this node can take a backup at all. Exported for
// the schedule route: a schedule is a promise to run backups later, so it has
// to answer the same question the backup verbs do rather than store a job every
// fire of which is known in advance to fail.
func (d *Daemon) BackupsReady() error { return d.backupsReady() }

// backupsReady reports whether this node can take a backup at all, and says
// which part is missing if it cannot.
func (d *Daemon) backupsReady() error {
	if d.cfg.Backups == nil || d.cfg.Blobs == nil {
		// Naming s3 and not fs: fs is a single-node store, and a clustered node
		// set to it refuses to boot at all (nodeboot.ArtifactStoreGuardError),
		// so "set it to fs" is advice that cannot be followed on the fleet
		// where this 501 is the default answer.
		return fmt.Errorf("%w: this node has no object store configured for backups — set "+
			"SANDBOXD_ARTIFACT_STORE=s3 with SANDBOXD_ARTIFACT_S3_ENDPOINT, "+
			"SANDBOXD_ARTIFACT_S3_BUCKET, SANDBOXD_ARTIFACT_S3_REGION, "+
			"SANDBOXD_ARTIFACT_S3_ACCESS_KEY and SANDBOXD_ARTIFACT_S3_SECRET_KEY "+
			"(fs is accepted too, but only on a standalone node, "+
			"and it puts the backup on the disk it is backing up)", ErrBackupsUnconfigured)
	}
	return nil
}

// BackupStarted is what a capture cost, returned alongside the row. PausedMs
// is the number an operator actually cares about — how long the machine stood
// still — and it is not in the row because it is a fact about one capture, not
// about the backup it produced.
type BackupStarted struct {
	store.Backup
	PausedMs int64
}

// Backup captures a running sandbox and uploads it. The capture is synchronous
// — the sandbox is running again before this returns — and the upload is not,
// so the response comes back in the time a pause takes rather than the time a
// gigabyte takes. The row is `pending` until the bytes are in the store.
//
// On Firecracker the first capture of a sandbox writes a full memory image and
// stands the guest still for it (measured: 2.6 s for a 1 GiB guest); every
// later one is a dirty-page diff against that base (129 ms). The engine
// decides, not this code — the same cost a checkpoint has always had.
func (d *Daemon) Backup(ctx context.Context, id string) (b BackupStarted, err error) {
	ctx, span := observability.StartInternal(ctx, "daemon.Backup", attribute.String("sandbox_id", id))
	defer func() { observability.End(span, err) }()

	if err := d.backupsReady(); err != nil {
		return BackupStarted{}, err
	}
	if err := d.refuseIfVolumes(ctx, id); err != nil {
		return BackupStarted{}, err
	}
	engine, err := d.checkpointer()
	if err != nil {
		return BackupStarted{}, err
	}
	sb, ok := d.lookup(id)
	if !ok {
		return BackupStarted{}, rt.ErrNotFound
	}
	d.mu.Lock()
	orgID := sb.OrgID
	d.mu.Unlock()

	// One capture at a time per sandbox. opMu would serialize them anyway; this
	// refuses the second rather than queueing a pause behind a gigabyte upload.
	if _, busy := d.cfg.Backups.PendingBackup(ctx, id); busy {
		return BackupStarted{}, ErrBackupInFlight
	}

	row, err := d.cfg.Backups.CreateBackup(ctx, store.Backup{
		OrgID: orgID, SandboxID: id, NodeID: d.cfg.NodeID,
		ExpiresAt: d.backupExpiry(id),
	})
	if err != nil {
		return BackupStarted{}, err
	}

	name := backupCheckpointName(row.ID)
	dir := filepath.Join(d.checkpointRoot(id), name)
	info, err := d.captureInto(ctx, engine, id, name, dir)
	if err != nil {
		// captureInto already removed the directory and the engine already
		// resumed the machine; the row is what is left to tell the truth.
		_ = d.cfg.Backups.SetBackupFailed(context.WithoutCancel(ctx), row.ID, err.Error())
		return BackupStarted{}, err
	}
	row.Kind = info.Kind

	// A backup's directory IS a checkpoint, and the restore path reads this
	// file to know whether the capture holds memory or only a disk. Without it
	// a restored backup is refused as "not ready" — the same guard that stops
	// a half-written checkpoint being restored.
	if err := writeCheckpointMeta(dir, d.readyCheckpoint(name, info)); err != nil {
		_ = os.RemoveAll(dir)
		_ = d.cfg.Backups.SetBackupFailed(context.WithoutCancel(ctx), row.ID, err.Error())
		return BackupStarted{}, err
	}

	// The upload outlives the request. WithoutCancel: a client that hangs up
	// after the pause must not leave the row pending forever.
	go d.uploadBackup(context.WithoutCancel(ctx), row, dir)

	d.log().Info("sandbox backup started", "sandbox_id", id, "backup", row.ID,
		"kind", info.Kind, "paused_ms", info.Paused.Milliseconds())
	return BackupStarted{Backup: row, PausedMs: info.Paused.Milliseconds()}, nil
}

// uploadBackup packs the capture, dedupes it against the sandbox's last ready
// backup and puts the bytes in the store. The capture directory is removed
// either way: it is a gigabyte on the node's disk whose only purpose was to be
// copied off it.
func (d *Daemon) uploadBackup(ctx context.Context, row store.Backup, dir string) {
	defer func() { _ = os.RemoveAll(dir) }()

	fail := func(err error) {
		d.log().Error("sandbox backup failed", "sandbox_id", row.SandboxID, "backup", row.ID, "error", err)
		_ = d.cfg.Backups.SetBackupFailed(ctx, row.ID, err.Error())
	}

	// A docker capture is a local image plus a file naming it; this is what
	// puts the image itself in the directory. Firecracker has nothing to do.
	if p, ok := d.cfg.Runtime.(rt.PortableCapture); ok {
		if err := p.Externalize(ctx, dir); err != nil {
			fail(fmt.Errorf("make the capture portable: %w", err))
			return
		}
	}

	packed, digest, size, err := packCapture(dir)
	if err != nil {
		fail(err)
		return
	}
	defer func() { _ = packed.Close(); _ = os.Remove(packed.Name()) }()

	// Dedupe: an unchanged capture is a row pointing at the bytes already in
	// the store, not a second copy of them. The parent is always a row that
	// owns its object, so a restore never walks a chain.
	if parent := d.dedupeParent(ctx, row.SandboxID, digest); parent != "" {
		if err := d.cfg.Backups.SetBackupReady(ctx, row.ID, row.Kind, "", digest, parent, size); err != nil {
			fail(err)
			return
		}
		d.log().Info("sandbox backup deduped", "sandbox_id", row.SandboxID,
			"backup", row.ID, "parent", parent, "bytes_saved", size)
		return
	}

	key := BackupKey(row.OrgID, row.SandboxID, row.ID)
	if err := d.cfg.Blobs.Put(ctx, key, packed, size); err != nil {
		fail(fmt.Errorf("upload backup: %w", err))
		return
	}
	if err := d.cfg.Backups.SetBackupReady(ctx, row.ID, row.Kind, key, digest, "", size); err != nil {
		fail(err)
		return
	}
	d.log().Info("sandbox backup ready", "sandbox_id", row.SandboxID, "backup", row.ID,
		"kind", row.Kind, "bytes", size, "store", d.cfg.Blobs.Name())
}

// dedupeParent is the backup this capture is byte-identical to, or "" to
// upload. The answer is always a row that owns its object: a deduped row holds
// no bytes, so pointing at one would need a chain walk on every restore, and
// pointing at one whose own parent is missing would make this row unrestorable
// as well. When the chain cannot be resolved the answer is "upload" — the
// saving is not worth a backup that is not one.
func (d *Daemon) dedupeParent(ctx context.Context, sandboxID, digest string) string {
	if digest == "" {
		return ""
	}
	last, ok := d.cfg.Backups.LastReadyBackup(ctx, sandboxID)
	if !ok || last.SHA256 != digest {
		return ""
	}
	if last.ObjectKey != "" {
		return last.ID
	}
	return last.ParentID
}

// ListBackups reports a sandbox's backups, newest first.
func (d *Daemon) ListBackups(ctx context.Context, orgID, id string) ([]store.Backup, error) {
	if err := d.backupsReady(); err != nil {
		return nil, err
	}
	return d.cfg.Backups.ListSandboxBackups(ctx, orgID, id)
}

// DeleteBackup removes a backup's object and then its row. The object first:
// a row without its object is invisible garbage an operator can still see and
// retry, an object without its row is unreclaimable garbage nothing points at.
func (d *Daemon) DeleteBackup(ctx context.Context, orgID, id, backupID string) error {
	if err := d.backupsReady(); err != nil {
		return err
	}
	row, ok := d.cfg.Backups.BackupByID(ctx, orgID, backupID)
	if !ok || row.SandboxID != id {
		return rt.ErrNotFound
	}
	if row.Status == store.BackupPending {
		return fmt.Errorf("%w, so it cannot be deleted yet", ErrBackupInFlight)
	}
	// Children first. store.DeleteBackup refuses a referenced row too, but by
	// then the object is already gone and the child it was protecting restores
	// from nothing — the guard has to run before the destructive step, not
	// beside it.
	children, err := d.cfg.Backups.BackupChildren(ctx, backupID)
	if err != nil {
		return err
	}
	if len(children) > 0 {
		return fmt.Errorf("%w: %s restores from it", store.ErrBackupReferenced, children[0])
	}
	if row.ObjectKey != "" {
		if err := d.cfg.Blobs.Delete(ctx, row.ObjectKey); err != nil && !isMissingObject(err) {
			return fmt.Errorf("delete backup object: %w", err)
		}
	}
	return d.cfg.Backups.DeleteBackup(ctx, orgID, backupID)
}

// RestoreBackup puts a backup's disk back onto the same sandbox: same id, name,
// preview URL, environment and start command. The sandbox's current disk is
// overwritten and there is no undo, which is why confirm is not a default.
func (d *Daemon) RestoreBackup(ctx context.Context, orgID, id, backupID string, confirm bool) (took time.Duration, err error) {
	ctx, span := observability.StartInternal(ctx, "daemon.RestoreBackup",
		attribute.String("sandbox_id", id), attribute.String("backup", backupID))
	defer func() { observability.End(span, err) }()

	if !confirm {
		return 0, ErrRestoreNotConfirmed
	}
	if err := d.backupsReady(); err != nil {
		return 0, err
	}
	if _, err := d.checkpointer(); err != nil {
		return 0, err
	}
	row, ok := d.cfg.Backups.BackupByID(ctx, orgID, backupID)
	if !ok || row.SandboxID != id {
		return 0, rt.ErrNotFound
	}
	if row.Status != store.BackupReady {
		return 0, fmt.Errorf("%w: it is %s", ErrBackupNotReady, row.Status)
	}
	// A capture in flight is reading the machine this restore would replace.
	if _, busy := d.cfg.Backups.PendingBackup(ctx, id); busy {
		return 0, ErrBackupInFlight
	}

	key := row.ObjectKey
	if key == "" {
		// A deduped row has no bytes of its own; its parent owns them.
		parent, ok := d.cfg.Backups.BackupByID(ctx, orgID, row.ParentID)
		if !ok || parent.ObjectKey == "" {
			return 0, fmt.Errorf("backup %s points at %s, which holds no object",
				row.ID, row.ParentID)
		}
		key = parent.ObjectKey
	}

	name := restoreCheckpointName(row.ID)
	dir := filepath.Join(d.checkpointRoot(id), name)

	// Removed only on a failure BEFORE the machine is replaced. A restored
	// Firecracker VM tracks dirty pages against the memory file in here
	// (vm.Sandbox.MemBase is src.MemPath until its next snapshot adopts it),
	// so deleting this directory on the way out makes the sandbox's *next*
	// capture fail with "copy memory base: no such file or directory" —
	// measured live, not reasoned about.
	materialised := false
	defer func() {
		if !materialised {
			_ = os.RemoveAll(dir)
		}
	}()

	body, err := d.cfg.Blobs.Get(ctx, key)
	if err != nil {
		return 0, fmt.Errorf("fetch backup %s: %w", row.ID, err)
	}
	err = unpackCapture(body, dir)
	_ = body.Close()
	if err != nil {
		return 0, fmt.Errorf("unpack backup %s: %w", row.ID, err)
	}
	if p, ok := d.cfg.Runtime.(rt.PortableCapture); ok {
		if err := p.Internalize(ctx, dir); err != nil {
			return 0, fmt.Errorf("load backup %s: %w", row.ID, err)
		}
	}
	materialised = true

	// From here it is a checkpoint restore: the same in-place rollback, with
	// the same rules about relaunching the start command for a disk capture.
	took, err = d.RestoreCheckpoint(ctx, id, name)
	if err != nil {
		return 0, err
	}
	// Only now: until the machine is actually replaced it may still be running
	// off an EARLIER materialisation, and sweeping that one before a restore
	// that then failed would break the surviving machine's next capture — the
	// same failure this directory's survival exists to prevent, on the error
	// path instead of the happy one.
	d.sweepRestoreDirs(id, name)

	d.log().Info("sandbox restored from backup", "sandbox_id", id, "backup", row.ID,
		"kind", row.Kind, "restore_ms", took.Milliseconds())
	return took, nil
}

// PruneBackups reclaims expired backups: the object first, then the row.
// Rows another backup's parent_id references are skipped by the query, so a
// deduped capture never loses the bytes it restores from.
//
// This lives on the daemon rather than in internal/retention because the
// object store is here: retention's other targets are one DELETE each, and a
// backup is two deletes in two systems that have to happen in that order.
func (d *Daemon) PruneBackups(ctx context.Context, defaultDays int) (int, error) {
	if err := d.backupsReady(); err != nil {
		return 0, nil // backups are off on this node; nothing to reclaim
	}
	expired, err := d.cfg.Backups.ExpiredBackups(ctx, d.clock.Now().Unix(), defaultDays, 100)
	if err != nil {
		return 0, err
	}
	pruned := 0
	for _, b := range expired {
		if b.Status == store.BackupPending {
			continue // a capture is still writing it
		}
		if b.ObjectKey != "" {
			if err := d.cfg.Blobs.Delete(ctx, b.ObjectKey); err != nil && !isMissingObject(err) {
				// Leave the row: it is the only pointer at the object, and
				// dropping it would make the bytes unreclaimable.
				d.log().Warn("could not reclaim a backup object", "backup", b.ID,
					"key", b.ObjectKey, "error", err)
				continue
			}
		}
		if err := d.cfg.Backups.DeleteBackup(ctx, b.OrgID, b.ID); err != nil {
			d.log().Warn("could not remove an expired backup row", "backup", b.ID, "error", err)
			continue
		}
		pruned++
	}
	if pruned > 0 {
		d.log().Info("reclaimed expired backups", "count", pruned)
	}
	return pruned, nil
}

// SetBackupRetention records how long this sandbox's backups are kept. Zero
// means the deployment default. Persisted at once, like SetHibernateTimeout:
// the backup cron job carries no retention column, so before this was written
// to the sandbox row a node restart reverted every sandbox to
// BACKUPS_RETAIN_DAYS with nothing said and nothing to read back.
func (d *Daemon) SetBackupRetention(ctx context.Context, id string, retain time.Duration) error {
	d.mu.Lock()
	sb, ok := d.boxes[id]
	if !ok {
		d.mu.Unlock()
		return rt.ErrNotFound
	}
	sb.BackupRetention = retain
	rec := d.recordOf(sb)
	d.mu.Unlock()

	d.persistRec(ctx, rec)
	return nil
}

// BackupRetention is this sandbox's own retention, or 0 when the deployment
// default decides. Read back so the config GET reports the number that will
// actually be stamped rather than the one the caller last sent.
func (d *Daemon) BackupRetention(id string) time.Duration {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if sb, ok := d.boxes[id]; ok {
		return sb.BackupRetention
	}
	return 0
}

// backupExpiry is when a new backup of this sandbox should be reclaimed, or 0
// to let the deployment default decide.
func (d *Daemon) backupExpiry(id string) int64 {
	if retain := d.BackupRetention(id); retain > 0 {
		return d.clock.Now().Add(retain).Unix()
	}
	return 0
}

// packCapture tars and gzips a capture directory into a temp file and returns
// it rewound, with the digest of the *tar* stream and the compressed size.
//
// gzip is what keeps a sparse disk from being sent as gigabytes of zeros: an
// unwritten extent reads as a zero run and compresses to almost nothing, on
// both runtimes and without a second on-disk format.
//
// ponytail: whole-file gzip, not an extent map, and measured rather than
// assumed. Packing a sparse file with 256 MiB of real data, against the same
// data with no holes around it (2026-09-22, APFS, gzip.BestSpeed):
//
//	logical  1 GiB -> 5.92s, 2.6 MB out;  data only 0.27s, 1.6 MB out
//	logical  4 GiB -> 7.41s, 6.5 MB out;  data only 0.29s, 1.6 MB out
//	logical  8 GiB -> 9.12s, 11.7 MB out; data only 0.28s, 1.6 MB out
//
// So a hole costs about 1.1 s and 1.2 MB per GiB — and none of it lands on the
// pause: packCapture runs in uploadBackup's goroutine, after the machine is
// running again, so paused_ms does not move. An extent map would buy back that
// second per gigabyte at the price of a second on-disk format, SEEK_DATA
// behind build tags, and a restore path that has to read both formats
// forever. Not worth it at 1 s/GiB of background CPU; revisit if a capture's
// pack time starts showing up as node load, which at these rates means disks
// in the hundreds of gigabytes.
//
// The digest is a manifest over the engine's files — name, length and content
// hash of each, in order — and not a hash of the archive. Two reasons: a gzip
// stream of the same input need not stay byte-identical across Go versions,
// and the daemon's own checkpoint.json carries this capture's id and
// timestamp, so hashing the archive would make every capture unique and
// nothing would ever dedupe.
func packCapture(dir string) (f *os.File, digest string, size int64, err error) {
	spool, err := os.CreateTemp("", "pd-backup-*.tar.gz")
	if err != nil {
		return nil, "", 0, fmt.Errorf("backup spool: %w", err)
	}
	// spool, not the named return: every error path below returns nil for f,
	// so cleaning up through f would dereference nil rather than remove the
	// temporary it meant to.
	defer func() {
		if err != nil {
			_ = spool.Close()
			_ = os.Remove(spool.Name())
		}
	}()

	manifest := sha256.New()
	zw, err := gzip.NewWriterLevel(spool, gzip.BestSpeed)
	if err != nil {
		return nil, "", 0, err
	}
	tw := tar.NewWriter(zw)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, "", 0, fmt.Errorf("read capture dir: %w", err)
	}
	// ReadDir is sorted by name, so the manifest order is stable.
	for _, e := range entries {
		if e.IsDir() {
			// A capture is a flat set of files on every engine; a directory
			// here is something else, and silently dropping it would restore a
			// machine missing a piece.
			return nil, "", 0, fmt.Errorf("capture holds a directory %q, which no engine writes", e.Name())
		}
		info, err := e.Info()
		if err != nil {
			return nil, "", 0, err
		}
		// Normalised: name, size and mode only. A timestamp or an owner would
		// make two captures of identical bytes hash differently and never dedupe.
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg, Name: e.Name(), Size: info.Size(), Mode: 0o600,
		}); err != nil {
			return nil, "", 0, err
		}
		src, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, "", 0, err
		}
		// Hashed while it is copied, so the file is read from disk once.
		fileSum := sha256.New()
		n, err := io.Copy(io.MultiWriter(tw, fileSum), src)
		_ = src.Close()
		if err != nil {
			return nil, "", 0, fmt.Errorf("pack %s: %w", e.Name(), err)
		}
		if n != info.Size() {
			return nil, "", 0, fmt.Errorf("pack %s: read %d of %d bytes", e.Name(), n, info.Size())
		}
		// The daemon's own record carries this capture's id and timestamp; it
		// is packed so the restore can read it, and left out of the digest so
		// two captures of an unchanged machine still compare equal.
		if e.Name() == ckptMetaFile {
			continue
		}
		fmt.Fprintf(manifest, "%s\x00%d\x00%x\n", e.Name(), n, fileSum.Sum(nil))
	}
	if err := tw.Close(); err != nil {
		return nil, "", 0, err
	}
	if err := zw.Close(); err != nil {
		return nil, "", 0, err
	}
	if err := spool.Sync(); err != nil {
		return nil, "", 0, err
	}
	st, err := spool.Stat()
	if err != nil {
		return nil, "", 0, err
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return nil, "", 0, err
	}
	return spool, hex.EncodeToString(manifest.Sum(nil)), st.Size(), nil
}

// unpackCapture reverses packCapture into dir, writing each file sparsely: a
// block of zeros is seeked over rather than written, so a restored overlay
// takes the space its data takes and not the space its disk claims.
func unpackCapture(body io.Reader, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	zr, err := gzip.NewReader(body)
	if err != nil {
		return fmt.Errorf("backup is not gzip: %w", err)
	}
	defer func() { _ = zr.Close() }() // the bytes are already written; a close error here says nothing new

	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		// The archive is written by packCapture, but it arrives over the
		// network from a store other tenants' nodes also write to: a name with
		// a separator in it would write outside dir. Rejected rather than
		// sanitised, so a crafted archive fails loudly instead of quietly
		// landing under a truncated name.
		if hdr.Name == "" || strings.ContainsAny(hdr.Name, `/\`) ||
			hdr.Name == "." || hdr.Name == ".." {
			return fmt.Errorf("backup holds an illegal entry name %q", hdr.Name)
		}
		// filepath.Base is defence in depth, not the check — on a name the
		// guard above accepted it is the identity. It is here so the path
		// handed to Join is provably a single element to a reader and to gosec.
		if err := writeSparse(filepath.Join(dir, filepath.Base(hdr.Name)), tr, hdr.Size); err != nil {
			return fmt.Errorf("unpack %s: %w", hdr.Name, err)
		}
	}
}

// writeSparse copies size bytes to path, skipping runs of zeros.
func writeSparse(path string, src io.Reader, size int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	// Sync below is what makes the write durable; this only releases the fd.
	defer func() { _ = f.Close() }()

	zero := make([]byte, backupBlockSize)
	buf := make([]byte, backupBlockSize)
	var off int64
	for off < size {
		n, err := io.ReadFull(src, buf[:min(int64(len(buf)), size-off)])
		if n > 0 {
			// A run of zeros is left as a hole: not writing it leaves it
			// unallocated, and the final truncate gives the file its length.
			if !bytes.Equal(buf[:n], zero[:n]) {
				if _, werr := f.WriteAt(buf[:n], off); werr != nil {
					return werr
				}
			}
			off += int64(n)
		}
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return err
		}
	}
	if off != size {
		return fmt.Errorf("short read: %d of %d bytes", off, size)
	}
	// The trailing hole has no bytes to seek past, so the length is set here.
	if err := f.Truncate(size); err != nil {
		return err
	}
	return f.Sync()
}

// isMissingObject reports an already-gone object, which a delete must treat as
// success: the point of the delete was that the bytes are not there.
func isMissingObject(err error) bool {
	return err != nil && strings.Contains(err.Error(), "artifact not found")
}
