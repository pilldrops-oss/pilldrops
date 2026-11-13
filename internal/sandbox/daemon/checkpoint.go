package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/pilldrops-oss/pilldrops/internal/observability"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
)

// MaxCheckpoints bounds a sandbox's save points. Each holds a full disk image,
// so the cap is what keeps one sandbox from filling the node.
const MaxCheckpoints = 10

// ckptDirSuffix and ckptMetaFile: the daemon owns the directory and its own
// metadata file. meta.json inside belongs to the engine (firecracker's restore
// reads it), so the daemon writes checkpoint.json beside it, last — a directory
// without it is a capture that did not finish.
const (
	ckptDirSuffix = ".ckpt"
	ckptMetaFile  = "checkpoint.json"
	ckptTmpSuffix = ".tmp"
)

// ExportDirPrefix marks the capture a named-snapshot save rides: the control
// plane cannot reach a node's disk, so it asks for a capture, streams it off
// and drops it. Exported because the HTTP layer names that directory.
const ExportDirPrefix = "export-"

// backupDirPrefix marks a backup's upload staging directory. restoreDirPrefix
// (backup.go) marks what a backup restore materialises into.
const backupDirPrefix = "bkp-"

// InternalCapture reports whether a capture directory belongs to the platform
// rather than to the sandbox's owner. All three are ordinary capture
// directories under the same root, and none of them is a save point anyone
// asked for: they are hidden from ListCheckpoints, so they neither show up in
// `checkpoint list` nor consume one of the MaxCheckpoints slots, they are
// exempt from that cap, and the user-facing checkpoint verbs refuse the names.
func InternalCapture(name string) bool {
	return strings.HasPrefix(name, restoreDirPrefix) ||
		strings.HasPrefix(name, backupDirPrefix) ||
		strings.HasPrefix(name, ExportDirPrefix)
}

// ErrCheckpointLimit means the sandbox already holds MaxCheckpoints save points.
var ErrCheckpointLimit = errors.New("checkpoint limit reached")

// ErrCheckpointNotReady means the capture never finished, so there is nothing to restore.
var ErrCheckpointNotReady = errors.New("checkpoint is not ready")

// maxCheckpointName is deliberately narrow: the name becomes a directory name
// and a docker image tag (sandbox.ValidLabel).
const maxCheckpointName = 40

// Checkpoint is one save point as the API reports it.
type Checkpoint struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	SizeBytes int64     `json:"size_bytes"`
	// Kind is "memory" (RAM and processes come back) or "disk" (filesystem only).
	Kind string `json:"kind"`
	// Status is "ready" or "building"; only a ready checkpoint can be restored.
	Status string `json:"status"`
	// PausedMs is how long the guest stood still for the capture; 0 on an engine
	// that captures without stopping it. Written into checkpoint.json, so a
	// listed save point still reports what its own capture cost.
	PausedMs int64 `json:"paused_ms,omitempty"`
}

// CheckpointDir is where one save point's files live. Exported because the
// snapshot-export API streams them off the node; nothing else should need it.
func (d *Daemon) CheckpointDir(id, name string) string {
	return filepath.Join(d.checkpointRoot(id), name)
}

// checkpointRoot is where one sandbox's save points live.
func (d *Daemon) checkpointRoot(id string) string {
	return filepath.Join(d.cfg.SnapshotDir, id+ckptDirSuffix)
}

// checkpointer returns the runtime's Checkpointer, or an ErrUnsupported naming it.
//
// Gated on the capability as well as the type assertion, for the same reason
// NamedSnapshot is: firecracker always implements the interface and still
// refuses every capture with FC_MEM_SNAPSHOTS off, so the bit the control plane
// schedules on is the bit the node refuses on.
func (d *Daemon) checkpointer() (rt.Checkpointer, error) {
	c, ok := d.cfg.Runtime.(rt.Checkpointer)
	if !ok || !d.cfg.Runtime.Capabilities().Checkpoint {
		return nil, fmt.Errorf("%w: the %s runtime cannot checkpoint", rt.ErrUnsupported, d.RuntimeName())
	}
	return c, nil
}

func validCheckpointName(name string) error {
	if !sandbox.ValidLabel(name, maxCheckpointName) {
		return fmt.Errorf("checkpoint name %q must be 1-%d characters of lowercase letters, "+
			"digits and hyphens, starting with a letter or digit", name, maxCheckpointName)
	}
	return nil
}

// captureInto runs the engine's capture of a sandbox into dir, which this call
// creates and the caller then owns. It holds opMu for the duration, so nothing
// else touches the machine while it stands still, and it removes dir on
// failure rather than leaving a half-written capture behind.
//
// Checkpoints and backups both go through here. One pause path means one set
// of rules about which states a capture is legal from, and a fix to the
// resume-on-failure shape cannot land in one caller and miss the other.
//
// A paused sandbox (every firecracker one after a node restart) is captured
// from its pause snapshot when the runtime can, else woken, captured and
// paused again: refusing it silently broke every scheduled backup.
func (d *Daemon) captureInto(ctx context.Context, engine rt.Checkpointer, id, name, dir string) (rt.CheckpointInfo, error) {
	info, err := d.captureOnce(ctx, engine, id, name, dir)
	if !errors.Is(err, errCaptureNeedsWake) {
		return info, err
	}
	before, _ := d.lastActive(id)
	if err := d.Resume(ctx, id); err != nil {
		return rt.CheckpointInfo{}, fmt.Errorf("wake the paused sandbox to capture it: %w", err)
	}
	woke, _ := d.lastActive(id)
	info, err = d.captureOnce(ctx, engine, id, name, dir)
	d.repauseIfUntouched(ctx, id, before, woke)
	return info, err
}

func (d *Daemon) lastActive(id string) (time.Time, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	sb, ok := d.boxes[id]
	if !ok {
		return time.Time{}, false
	}
	return sb.LastActiveAt, true
}

// repauseIfUntouched puts a sandbox a capture woke back to sleep, unless its
// owner used it meanwhile (its idle clock moved past the wake). The idle clock
// goes back to before the wake, or a scheduled backup would keep an abandoned
// sandbox alive forever, and the capture's running time is not metered.
// ponytail: a usage flush that samples mid-capture still bills that slice;
// pausing the meter for the capture needs a meter state of its own.
func (d *Daemon) repauseIfUntouched(ctx context.Context, id string, before, woke time.Time) {
	d.mu.Lock()
	sb, ok := d.boxes[id]
	untouched := ok && sb.State == StateRunning && sb.LastActiveAt.Equal(woke)
	if untouched {
		sb.LastActiveAt = before
		sb.meter.resume(d.clock.Now())
	}
	d.mu.Unlock()
	if !untouched {
		d.log().Info("capture: the sandbox was used while awake for it; leaving it running", "sandbox_id", id)
		return
	}
	if err := d.Pause(ctx, id); err != nil {
		d.log().Warn("capture: pausing again after the capture failed", "sandbox_id", id, "error", err)
	}
}

// errCaptureNeedsWake is a paused sandbox whose runtime captures only running ones.
var errCaptureNeedsWake = errors.New("paused sandbox must be woken to capture")

func (d *Daemon) captureOnce(ctx context.Context, engine rt.Checkpointer, id, name, dir string) (rt.CheckpointInfo, error) {
	sb, release, err := d.acquireOp(id)
	if err != nil {
		return rt.CheckpointInfo{}, err
	}
	defer release()

	d.mu.Lock()
	st, inst, snap := sb.State, sb.inst, sb.Snapshot
	d.mu.Unlock()
	paused, fromSnapshot := engine.(rt.PausedCheckpointer)
	switch {
	case st == StateRunning:
	case st == StatePaused && fromSnapshot:
	case st == StatePaused:
		return rt.CheckpointInfo{}, errCaptureNeedsWake
	default:
		return rt.CheckpointInfo{}, fmt.Errorf("cannot checkpoint from state %s", st)
	}

	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return rt.CheckpointInfo{}, fmt.Errorf("checkpoint dir: %w", err)
	}
	dst := rt.SnapshotRef{Path: dir, Name: name}
	var info rt.CheckpointInfo
	if st == StatePaused {
		info, err = paused.CheckpointPaused(ctx, id, snap, dst)
	} else {
		info, err = engine.Checkpoint(ctx, inst, dst)
	}
	if err != nil {
		_ = os.RemoveAll(dir)
		return rt.CheckpointInfo{}, fmt.Errorf("checkpoint: %w", err)
	}
	return info, nil
}

// readyCheckpoint describes a finished capture. A checkpoint and a backup write
// the same record: a backup's directory IS a checkpoint directory.
func (d *Daemon) readyCheckpoint(name string, info rt.CheckpointInfo) Checkpoint {
	return Checkpoint{
		Name: name, CreatedAt: d.clock.Now().UTC(), SizeBytes: info.SizeBytes,
		Kind: info.Kind, Status: "ready", PausedMs: info.Paused.Milliseconds(),
	}
}

// Checkpoint saves a named save point of a running sandbox. The sandbox keeps
// running. Saving over an existing name replaces it atomically, so a failed
// re-save never destroys the point it was replacing.
func (d *Daemon) Checkpoint(ctx context.Context, id, name string) (c Checkpoint, err error) {
	ctx, span := observability.StartInternal(ctx, "daemon.Checkpoint",
		attribute.String("sandbox_id", id), attribute.String("checkpoint", name))
	defer func() { observability.End(span, err) }()

	if err := validCheckpointName(name); err != nil {
		return Checkpoint{}, err
	}
	engine, err := d.checkpointer()
	if err != nil {
		return Checkpoint{}, err
	}
	existing, _ := d.ListCheckpoints(id)
	replacing := slices.ContainsFunc(existing, func(c Checkpoint) bool { return c.Name == name })
	// The cap protects the node from one sandbox's save points. A snapshot save
	// is not one of those: refusing it here failed the export after its 202 had
	// gone out, so a sandbox holding ten checkpoints could never be snapshotted.
	if !replacing && !InternalCapture(name) && len(existing) >= MaxCheckpoints {
		return Checkpoint{}, fmt.Errorf("%w: %d per sandbox", ErrCheckpointLimit, MaxCheckpoints)
	}

	final := filepath.Join(d.checkpointRoot(id), name)
	tmp := final + ckptTmpSuffix
	info, err := d.captureInto(ctx, engine, id, name, tmp)
	if err != nil {
		return Checkpoint{}, err
	}
	c = d.readyCheckpoint(name, info)
	if err := writeCheckpointMeta(tmp, c); err != nil {
		_ = os.RemoveAll(tmp)
		return Checkpoint{}, err
	}
	// Swap last: until the rename the old point under this name is still whole.
	_ = os.RemoveAll(final)
	if err := os.Rename(tmp, final); err != nil {
		_ = os.RemoveAll(tmp)
		return Checkpoint{}, fmt.Errorf("publish checkpoint: %w", err)
	}

	sb, ok := d.lookup(id)
	if !ok {
		return Checkpoint{}, rt.ErrNotFound
	}
	d.mu.Lock()
	orgID, template := sb.OrgID, sb.Template
	d.mu.Unlock()
	d.recordEvent(ctx, orgID, id, "checkpointed", template)
	d.log().Info("sandbox checkpointed", "sandbox_id", id, "checkpoint", name,
		"kind", info.Kind, "paused_ms", info.Paused.Milliseconds(), "size_bytes", info.SizeBytes)
	return c, nil
}

// ListCheckpoints reports a sandbox's save points, oldest name first. A
// directory with no checkpoint.json is a capture that was interrupted; it is
// listed as "building" so an operator can see the disk it holds, and refused
// by a restore.
func (d *Daemon) ListCheckpoints(id string) ([]Checkpoint, error) {
	if _, ok := d.lookup(id); !ok {
		return nil, rt.ErrNotFound
	}
	entries, err := os.ReadDir(d.checkpointRoot(id))
	if err != nil {
		if os.IsNotExist(err) {
			return []Checkpoint{}, nil
		}
		return nil, err
	}
	out := make([]Checkpoint, 0, len(entries))
	for _, e := range entries {
		// A backup's staging and materialisation and a snapshot export live
		// here too, and none is a save point the owner took: hiding them keeps
		// them out of the list and out of the MaxCheckpoints count.
		if !e.IsDir() || strings.HasSuffix(e.Name(), ckptTmpSuffix) ||
			InternalCapture(e.Name()) {
			continue
		}
		c, err := readCheckpointMeta(filepath.Join(d.checkpointRoot(id), e.Name()))
		if err != nil {
			c = Checkpoint{Name: e.Name(), Status: "building"}
		}
		c.Name = e.Name() // the directory is the authority on the name
		out = append(out, c)
	}
	// Already name-ordered: os.ReadDir sorts its entries.
	return out, nil
}

// RemoveCheckpoint deletes one save point. Removing one that is not there is an error,
// so a typo is not reported as success.
func (d *Daemon) RemoveCheckpoint(id, name string) error {
	if err := validCheckpointName(name); err != nil {
		return err
	}
	if _, ok := d.lookup(id); !ok {
		return rt.ErrNotFound
	}
	dir := filepath.Join(d.checkpointRoot(id), name)
	if _, err := os.Stat(dir); err != nil {
		return rt.ErrNotFound
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	d.log().Info("checkpoint removed", "sandbox_id", id, "checkpoint", name)
	return nil
}

// RestoreCheckpoint rolls a sandbox back to a save point in place: same id, URL,
// token and address. A memory checkpoint comes back with its processes still
// running, so the start command is not relaunched; a disk one has lost them and
// is relaunched like a resume.
func (d *Daemon) RestoreCheckpoint(ctx context.Context, id, name string) (took time.Duration, err error) {
	ctx, span := observability.StartInternal(ctx, "daemon.RestoreCheckpoint",
		attribute.String("sandbox_id", id), attribute.String("checkpoint", name))
	defer func() { observability.End(span, err) }()

	if !d.isolationOK.Load() {
		return 0, ErrIsolationPreflight
	}
	if err := validCheckpointName(name); err != nil {
		return 0, err
	}
	engine, err := d.checkpointer()
	if err != nil {
		return 0, err
	}
	dir := filepath.Join(d.checkpointRoot(id), name)
	meta, err := readCheckpointMeta(dir)
	if err != nil {
		if os.IsNotExist(err) {
			if _, serr := os.Stat(dir); serr == nil {
				return 0, ErrCheckpointNotReady
			}
			return 0, rt.ErrNotFound
		}
		return 0, err
	}
	// A paused sandbox has no instance to replace, and the engine needs one to
	// know which machine this is. Wake it first, outside opMu, then re-check.
	if err := d.ensureRunning(ctx, id); err != nil {
		return 0, err
	}

	sb, release, err := d.acquireOp(id)
	if err != nil {
		return 0, err
	}
	defer release()

	d.mu.Lock()
	if sb.State != StateRunning {
		st := sb.State
		d.mu.Unlock()
		return 0, fmt.Errorf("cannot restore from state %s", st)
	}
	// Resuming, not Pausing: ensureRunning callers wait out a restore instead of
	// starting a second one.
	sb.State = StateResuming
	inst := sb.inst
	u := sb.meter.sample(d.clock.Now())
	sb.meter.freeze(d.clock.Now())
	orgID, template, image, ports := sb.OrgID, sb.Template, sb.Image, sb.Ports
	startCmd, startEnv := sb.StartCmd, sb.Env
	vcpu, memMiB, storageMB := sb.VCPU, sb.MemMiB, sb.StorageMB
	d.mu.Unlock()
	d.flushUsage(ctx, id, orgID, u)

	if vcpu <= 0 {
		vcpu = d.cfg.DefaultVCPU
	}
	if memMiB <= 0 {
		memMiB = d.cfg.DefaultMem
	}

	// The VMM is torn down inside this call; a caller that gives up must not
	// leave the sandbox half-restored.
	restoreCtx := context.WithoutCancel(ctx)
	t0 := d.clock.Now()
	newInst, err := engine.RestoreCheckpoint(restoreCtx, inst, rt.SnapshotRef{Path: dir, Name: name}, rt.BootSpec{
		SandboxID: id,
		Template:  rt.Template{Name: template, Image: image, Ports: ports},
		VCPU:      vcpu, MemMiB: memMiB, StorageMB: storageMB,
	})
	if err != nil {
		d.mu.Lock()
		// Failed, not paused: the old machine is gone and there is no snapshot to
		// resume from. Destroy still reclaims the disk, namespace and records.
		sb.State = StateFailed
		d.mu.Unlock()
		return 0, fmt.Errorf("restore checkpoint: %w", err)
	}
	took = d.clock.Now().Sub(t0)
	d.metrics.ResumeDuration.Observe(took)

	d.mu.Lock()
	sb.inst = newInst
	sb.State = StateRunning
	sb.LastActiveAt = d.clock.Now()
	sb.meter.resume(d.clock.Now())
	rec := d.recordOf(sb)
	d.mu.Unlock()

	// A memory checkpoint brought the app back with the rest of the guest.
	if meta.Kind != "memory" && startCmd != "" {
		if err := newInst.StartServer(ctx, startCmd, startEnv); err != nil {
			d.log().Warn("restore checkpoint: app restart failed", "sandbox_id", id, "error", err)
		}
	}
	d.persistRec(ctx, rec)
	d.recordEvent(ctx, orgID, id, "checkpoint_restored", template)
	d.log().Info("sandbox restored from checkpoint", "sandbox_id", id, "checkpoint", name,
		"kind", meta.Kind, "restore_ms", took.Milliseconds())
	return took, nil
}

// sweepCheckpoints removes checkpoint directories for sandboxes this daemon no
// longer has a record of. Called once at start, after the store is read.
func (d *Daemon) sweepCheckpoints() {
	entries, err := os.ReadDir(d.cfg.SnapshotDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasSuffix(e.Name(), ckptDirSuffix) {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ckptDirSuffix)
		if _, ok := d.lookup(id); ok {
			continue
		}
		dir := filepath.Join(d.cfg.SnapshotDir, e.Name())
		if err := os.RemoveAll(dir); err != nil {
			d.log().Warn("stray checkpoint dir remove failed", "dir", dir, "error", err)
			continue
		}
		d.log().Info("removed stray checkpoint dir", "sandbox_id", id, "dir", dir)
	}
}

func writeCheckpointMeta(dir string, c Checkpoint) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	// 0600: the daemon's own bookkeeping, and nothing in the guest reads it.
	return os.WriteFile(filepath.Join(dir, ckptMetaFile), b, 0o600)
}

func readCheckpointMeta(dir string) (Checkpoint, error) {
	var c Checkpoint
	b, err := os.ReadFile(filepath.Join(dir, ckptMetaFile))
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(b, &c)
}
