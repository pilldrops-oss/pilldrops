package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/observability"
	"github.com/pilldrops-oss/pilldrops/internal/store"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
	"go.opentelemetry.io/otel/attribute"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

func (d *Daemon) lookup(id string) (*Sandbox, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	sb, ok := d.boxes[id]
	return sb, ok
}

// acquireOp is the preamble every per-sandbox operation shares: find the
// sandbox, take its op lock, and re-check it was not destroyed while waiting
// for that lock. The returned release drops the op lock. d.mu is not held on
// return — the caller takes it for its own state check, which is safe because
// Destroy also goes through here and so cannot run in between.
func (d *Daemon) acquireOp(id string) (*Sandbox, func(), error) {
	sb, ok := d.lookup(id)
	if !ok {
		return nil, nil, rt.ErrNotFound
	}
	sb.opMu.Lock()
	d.mu.Lock()
	_, alive := d.boxes[id]
	d.mu.Unlock()
	if !alive {
		sb.opMu.Unlock()
		return nil, nil, rt.ErrNotFound
	}
	return sb, sb.opMu.Unlock, nil
}

// pauseRecordTimeout bounds the bookkeeping after a pause's snapshot, however
// little of the caller's deadline is left.
const pauseRecordTimeout = 5 * time.Second

// Pause snapshots and stops a running sandbox's instance. Serialized per
// sandbox on opMu and idempotent.
func (d *Daemon) Pause(ctx context.Context, id string) error {
	_, err := d.PauseTimed(ctx, id)
	return err
}

// PauseTimed is Pause reporting how long the snapshot took; zero when the
// sandbox was already paused.
func (d *Daemon) PauseTimed(ctx context.Context, id string) (took time.Duration, err error) {
	ctx, span := observability.StartInternal(ctx, "daemon.Pause", attribute.String("sandbox_id", id))
	defer func() { observability.End(span, err) }()

	// A pause snapshot names this node's own build of the root image and
	// leaves the disk overlay where it is, so another node refused to boot one.
	// An archive another node may restore from gets the capture a move ships
	// instead, taken running just before the pause (Checkpoint takes the op
	// lock itself, so here, before it).
	capDir := ""
	if d.portableArchive() && !shuttingDown(ctx) {
		if _, cerr := d.Checkpoint(ctx, id, pauseCaptureName); cerr == nil {
			capDir = d.CheckpointDir(id, pauseCaptureName)
			if lerr := addCaptureRootfs(capDir); lerr != nil {
				d.log().Warn("pause: capture has no root image, so no other node could boot it; not archiving it",
					"sandbox_id", id, "error", lerr)
				_ = os.RemoveAll(capDir)
				capDir = ""
			}
		} else {
			d.log().Info("pause: no portable capture, so this pause is not archived",
				"sandbox_id", id, "error", cerr)
		}
	}

	sb, release, err := d.acquireOp(id)
	if err != nil {
		return 0, err
	}
	defer release()

	d.mu.Lock()
	switch sb.State {
	case StatePaused:
		d.mu.Unlock()
		return 0, nil
	case StateRunning, StateStandby:
	default:
		st := sb.State
		d.mu.Unlock()
		return 0, fmt.Errorf("cannot pause from state %s", st)
	}
	wasStandby := sb.State == StateStandby
	sb.State = StatePausing
	inst := sb.inst
	d.mu.Unlock()

	// Thaw first: the snapshot path pauses the VM itself, and a VMM that is
	// already frozen is not a case anything has measured.
	// ponytail: snapshotting straight from the frozen VMM would skip this; do it
	// when the thaw shows up in PauseDuration.
	if wasStandby {
		if fr, ok := inst.(rt.Freezer); ok {
			if err := fr.Thaw(ctx); err != nil {
				d.mu.Lock()
				sb.State = StateStandby
				d.mu.Unlock()
				return 0, fmt.Errorf("thaw before pause: %w", err)
			}
			d.mu.Lock()
			sb.meter.resume(d.clock.Now())
			d.mu.Unlock()
		}
	}

	snap := rt.SnapshotRef{Path: d.snapshotPath(id)}
	snapStart := d.clock.Now()
	snapCtx, snapSpan := observability.StartInternal(ctx, "runtime.Snapshot", attribute.String("sandbox_id", id))
	if err := inst.Snapshot(snapCtx, snap); err != nil {
		observability.End(snapSpan, err)
		d.mu.Lock()
		sb.State = StateRunning
		d.mu.Unlock()
		return 0, fmt.Errorf("snapshot: %w", err)
	}
	observability.End(snapSpan, nil)
	took = d.clock.Now().Sub(snapStart)
	d.metrics.PauseDuration.Observe(took)
	// From here the instance is gone and only the record leads back to the
	// snapshot, so a caller's deadline expiring must not lose the sandbox.
	recordCtx, cancelRecord := context.WithTimeout(context.WithoutCancel(ctx), pauseRecordTimeout)
	defer cancelRecord()
	if err := inst.Stop(recordCtx); err != nil {
		// The snapshot succeeded, so still mark paused rather than strand it as running.
		d.log().Warn("pause: instance would not stop", "sandbox_id", id, "error", err)
	}

	d.mu.Lock()
	u := sb.meter.sample(d.clock.Now())
	sb.meter.freeze(d.clock.Now())
	sb.State = StatePaused
	sb.Snapshot = snap
	sb.pauseArchive = "" // this pause supersedes any archived one until it is archived itself
	sb.inst = nil
	// The namespace went with the instance, and the address it held is free for
	// another sandbox. Keep the tags — resume rejoins — but stop reporting an
	// address nothing is listening on.
	sb.PrivateIP = ""
	orgID, template := sb.OrgID, sb.Template
	rec := d.recordOf(sb)
	d.mu.Unlock()

	d.flushUsage(recordCtx, id, orgID, u)
	d.persistRec(recordCtx, rec)
	if d.portableArchive() {
		if capDir != "" {
			if pid, ok := d.archivePause(ctx, id, capDir); ok {
				d.mu.Lock()
				sb.pauseArchive = pid
				rec := d.recordOf(sb)
				d.mu.Unlock()
				d.persistRec(ctx, rec)
			}
			_ = os.RemoveAll(capDir)
		}
	} else {
		d.archiveSnapshot(ctx, id, snap.Path)
	}
	d.recordEvent(recordCtx, orgID, id, "paused", template)
	d.revokeConsoleTokens(recordCtx, id)
	d.log().Info("sandbox paused", "sandbox_id", id, "org_id", orgID, "paused_ms", took.Milliseconds())
	return took, nil
}

// Resume restores a paused sandbox to running. Serialized per sandbox on opMu
// and idempotent, so concurrent callers collapse to one restore.
func (d *Daemon) Resume(ctx context.Context, id string) error {
	_, err := d.ResumeTimed(ctx, id)
	return err
}

// ResumeTimed is Resume reporting how long the restore took; zero when the
// sandbox was already running.
func (d *Daemon) ResumeTimed(ctx context.Context, id string) (took time.Duration, err error) {
	ctx, span := observability.StartInternal(ctx, "daemon.Resume", attribute.String("sandbox_id", id))
	defer func() { observability.End(span, err) }()

	// Guard here, not in the HTTP handler: the unauthenticated preview wake path also resumes.
	if !d.isolationOK.Load() {
		return 0, ErrIsolationPreflight
	}
	sb, release, err := d.acquireOp(id)
	if err != nil {
		return 0, err
	}
	defer release()

	d.mu.Lock()
	if sb.State == StateRunning {
		// Still a caller asking for it: activity, which a capture that woke
		// the sandbox reads before putting it back to sleep.
		sb.LastActiveAt = d.clock.Now()
		d.mu.Unlock()
		return 0, nil
	}
	if !canTransition(sb.State, "resume") {
		st := sb.State
		d.mu.Unlock()
		if st == StateFailed {
			return 0, rt.ErrUnresumable
		}
		return 0, fmt.Errorf("cannot resume from state %s", st)
	}
	// Standby is the cheap half: the instance is still there, frozen. Thaw it
	// and do not go near the restore path, which would boot a second machine.
	if sb.State == StateStandby {
		inst := sb.inst
		sb.State = StateResuming
		d.mu.Unlock()
		return d.thaw(ctx, id, sb, inst)
	}
	sb.State = StateResuming
	snap := sb.Snapshot
	template, image, ports := sb.Template, sb.Image, sb.Ports
	startCmd := sb.StartCmd
	startEnv := sb.Env
	vcpu := sb.VCPU
	if vcpu <= 0 {
		vcpu = d.cfg.DefaultVCPU
	}
	memMiB := sb.MemMiB
	if memMiB <= 0 {
		memMiB = d.cfg.DefaultMem
	}
	storageMB := sb.StorageMB
	d.mu.Unlock()

	// A node that was cut off long enough to be reaped may have had this
	// sandbox rehomed elsewhere while it still held its copy. That copy is the
	// older one: forget it rather than boot a second, stale machine.
	if o, ok := d.cfg.Store.(ownerChecker); ok {
		if elsewhere, err := o.OwnedElsewhere(ctx, id); err == nil && elsewhere {
			d.mu.Lock()
			d.forget(id)
			d.mu.Unlock()
			d.log().Warn("resume refused: this sandbox was rehomed to another node while this one was away",
				"sandbox_id", id)
			return 0, rt.ErrNotFound
		}
	}

	restoreStart := d.clock.Now()
	restoreCtx, restoreSpan := observability.StartInternal(ctx, "runtime.Restore", attribute.String("sandbox_id", id))
	inst, err := d.cfg.Runtime.Restore(restoreCtx, snap, rt.BootSpec{
		SandboxID: id,
		Template:  rt.Template{Name: template, Image: image, Ports: ports},
		VCPU:      vcpu, MemMiB: memMiB, StorageMB: storageMB,
	})
	observability.End(restoreSpan, err)
	if err != nil {
		unresumable := errors.Is(err, rt.ErrUnresumable)
		d.mu.Lock()
		sb.State = StatePaused // revert so a retry can resume again
		if unresumable {
			sb.State = StateFailed
		}
		rec := d.recordOf(sb)
		d.mu.Unlock()
		if unresumable {
			d.persistRec(ctx, rec)
		}
		return 0, fmt.Errorf("restore: %w", err)
	}

	took = d.clock.Now().Sub(restoreStart)
	d.metrics.ResumeDuration.Observe(took)

	d.mu.Lock()
	sb.inst = inst
	sb.State = StateRunning
	sb.LastActiveAt = d.clock.Now()
	sb.meter.resume(d.clock.Now())
	orgID := sb.OrgID
	rec := d.recordOf(sb)
	d.mu.Unlock()

	// A resume builds a new namespace at a new slot, so the membership has to be
	// re-pushed or the sandbox comes back on no network. Logged, not fatal: a
	// woken sandbox that cannot reach its peers is better than one that will
	// not wake.
	if err := d.pushNetworks(ctx, id); err != nil {
		d.log().Warn("resume: could not restore network membership", "sandbox_id", id, "error", err)
	}
	// Relaunch the published app; a failure is logged, not fatal. A memory
	// restore brought it back with the rest of the guest, so relaunching would
	// leave two copies fighting over the port — the same guard RestoreCheckpoint
	// makes on meta.Kind, which Restore has no meta to read.
	if startCmd != "" && !d.cfg.Runtime.Capabilities().MemoryRestore {
		if err := inst.StartServer(ctx, startCmd, startEnv); err != nil {
			d.log().Warn("resume: app restart failed", "sandbox_id", id, "error", err)
		}
	}
	d.persistRec(ctx, rec)
	d.recordEvent(ctx, orgID, id, "resumed", template)
	d.discardArchive(id) // it runs again; its record already refuses the archive
	d.log().Info("sandbox resumed", "sandbox_id", id, "org_id", orgID, "resume_ms", took.Milliseconds())
	return took, nil
}

// errNoLongerDead is a reap called off because the sandbox changed or answered.
var errNoLongerDead = errors.New("sandbox is no longer the dead instance the scan found")

// destroyLogCaptureTimeout is a var so tests can shrink it.
var destroyLogCaptureTimeout = 5 * time.Second

// Destroy stops and forgets a sandbox, flushing final usage. Serialized per
// sandbox on opMu. Every guest-facing step is deadline-bounded or killable from
// the host, so no guest state can make DELETE hang.
func (d *Daemon) Destroy(ctx context.Context, id string) (err error) {
	return d.destroy(ctx, id, false, "destroyed", nil)
}

// Evict stops and forgets a sandbox that now runs on another node under the
// same id. Everything keyed by the id (its row, history, archived snapshot,
// agent tokens) belongs to the moved copy, so only this node's instance, its
// local files and its final usage are dealt with.
func (d *Daemon) Evict(ctx context.Context, id string) error {
	return d.destroy(ctx, id, true, "", nil)
}

// exitState names how the sandbox ended, in its history row and its last event.
// dead, when set, is the instance a scan found dead without the op lock; the
// destroy goes ahead only if it is still this sandbox's running instance and
// still dead, since a pause or a checkpoint restore may have run in between.
func (d *Daemon) destroy(ctx context.Context, id string, evict bool, exitState string, dead rt.Liveness) (err error) {
	ctx, span := observability.StartInternal(ctx, "daemon.Destroy", attribute.String("sandbox_id", id))
	defer func() {
		if err == nil && !evict {
			d.detachVolumes(ctx, id)
		}
	}()
	defer func() { observability.End(span, err) }()

	sb, release, err := d.acquireOp(id)
	if err != nil {
		return err
	}
	defer release()

	if dead != nil {
		d.mu.Lock()
		lv, ok := sb.inst.(rt.Liveness)
		current := ok && lv == dead && sb.State == StateRunning
		d.mu.Unlock()
		if !current || dead.Alive() {
			return errNoLongerDead
		}
	}

	d.mu.Lock()
	u := sb.meter.sample(d.clock.Now())
	life := sb.meter.total()
	inst := sb.inst
	snap := sb.Snapshot
	// Copy what the history record needs under the lock; sb is unowned after the unlock.
	orgID, template, createdAt := sb.OrgID, sb.Template, sb.CreatedAt
	vcpu, memMiB := sb.VCPU, sb.MemMiB
	unpin := sb.unpin
	sb.unpin = nil
	sb.inst = nil
	sb.State = StateStopped
	d.forget(id)
	// A rehome of this id started once it is forgotten would stage a capture
	// the teardown below removes: it waits for the teardown. Not taken when a
	// rehome already holds the id, as when its own boot failed and destroys.
	if _, busy := d.rehoming[id]; !busy {
		done := make(chan struct{})
		d.rehoming[id] = done
		defer func() {
			d.mu.Lock()
			delete(d.rehoming, id)
			d.mu.Unlock()
			close(done)
		}()
	}
	d.mu.Unlock()

	// The sandbox is gone from d.boxes, so teardown must finish even if the
	// caller gives up; otherwise a live VMM and DB record are left behind.
	ctx = context.WithoutCancel(ctx)

	// Before the instance goes: the address this sandbox holds is about to be
	// reused, and a membership that outlives it would hand its peers to whoever
	// takes the address next.
	if inst != nil {
		d.dropNetworks(ctx, inst, id)
	}

	var histLogs string
	if inst != nil {
		// Best-effort and bounded: a wedged or VMM-dead guest is a TCP black hole,
		// and an unbounded read here hangs DELETE for minutes.
		logCtx, cancelLog := context.WithTimeout(ctx, destroyLogCaptureTimeout)
		if data, err := inst.FS().Read(logCtx, rt.AppLogPath); err == nil {
			histLogs = string(data)
		} else {
			d.log().Debug("destroy: app-log capture skipped", "sandbox_id", id, "error", err)
		}
		cancelLog()
		if dead == nil {
			d.flushVolumes(ctx, id, inst)
		}
		if err := inst.Stop(ctx); err != nil {
			d.log().Warn("destroy: instance would not stop", "sandbox_id", id, "error", err)
		}
	}
	// Unpin only after the instance is down; the pin protects the rootfs the VM has open.
	if unpin != nil {
		unpin()
	}
	// Outside the inst != nil block: a paused sandbox has no inst but still has a snapshot.
	if sd, ok := d.cfg.Runtime.(rt.SnapshotDeleter); ok {
		if err := sd.DeleteSnapshot(ctx, id, snap); err != nil {
			d.log().Warn("destroy: snapshot delete failed", "sandbox_id", id, "error", err)
		}
	}
	// The pause snapshot (state + memory image) is the daemon's own file; a
	// resumed sandbox still maps its mem file, so this waits for inst.Stop above.
	if snap.Path != "" {
		if err := os.RemoveAll(snap.Path); err != nil {
			d.log().Warn("destroy: snapshot dir remove failed", "sandbox_id", id, "error", err)
		}
	}
	// Checkpoints die with the sandbox; one that outlives it is add-named-snapshots.
	if err := os.RemoveAll(d.checkpointRoot(id)); err != nil {
		d.log().Warn("destroy: checkpoint dir remove failed", "sandbox_id", id, "error", err)
	}
	d.removeCaptureIfUnused(ctx, orgID, template, id)
	d.flushUsage(ctx, id, orgID, u)
	if evict {
		d.liveStats.forget(id)
		d.diskSample.forget(id)
		d.log().Info("sandbox moved off this node", "sandbox_id", id, "org_id", orgID)
		return nil
	}
	if d.cfg.Store != nil {
		now := d.clock.Now()
		if err := d.cfg.Store.RecordHistory(ctx, SandboxHistory{
			ID: id, OrgID: orgID, Template: template, Node: d.cfg.NodeID, ExitState: exitState,
			CreatedAt: createdAt.Unix(), DestroyedAt: now.Unix(),
			DurationSecs: int64(now.Sub(createdAt).Seconds()),
			VCPU:         vcpu, MemMiB: memMiB,
			Usage: life, Logs: histLogs,
		}); err != nil {
			d.log().Error("record history failed", "sandbox_id", id, "org_id", orgID, "error", err)
		}
		if err := d.cfg.Store.DeleteSandbox(ctx, id); err != nil {
			d.log().Error("destroy: record delete failed", "sandbox_id", id, "error", err)
		}
	}
	if d.cfg.SnapshotArchive != nil {
		if err := d.cfg.SnapshotArchive.Delete(ctx, id); err != nil {
			d.log().Warn("destroy: archived snapshot delete failed", "sandbox_id", id, "error", err)
		}
	}
	// Not after a crash: the backups are then the only copy of the owner's data.
	if dead == nil {
		d.dropBackups(ctx, orgID, id)
	}
	d.liveStats.forget(id)
	d.diskSample.forget(id)
	d.revokeAgentTokens(ctx, id)
	d.recordEvent(ctx, orgID, id, exitState, template)
	d.log().Info("sandbox destroyed", "sandbox_id", id, "org_id", orgID)
	return nil
}

// SetTimeout sets a new hard TTL from now. A negative duration clears the TTL,
// making the sandbox persistent.
func (d *Daemon) SetTimeout(ctx context.Context, id string, hard time.Duration) error {
	d.mu.Lock()
	sb, ok := d.boxes[id]
	if !ok {
		d.mu.Unlock()
		return rt.ErrNotFound
	}
	if hard < 0 {
		sb.ExpiresAt = time.Time{}
	} else {
		sb.ExpiresAt = d.clock.Now().Add(hard)
	}
	// Persist now, or a restart restores the stale ExpiresAt and reaps the sandbox.
	rec := d.recordOf(sb)
	d.mu.Unlock()

	d.persistRec(ctx, rec)
	return nil
}

// SetIdleTimeout changes a live sandbox's idle auto-pause: 0 = daemon default,
// negative = never. Persisted at once, like SetTimeout.
func (d *Daemon) SetIdleTimeout(ctx context.Context, id string, idle time.Duration) error {
	d.mu.Lock()
	sb, ok := d.boxes[id]
	if !ok {
		d.mu.Unlock()
		return rt.ErrNotFound
	}
	sb.IdleTimeout = idle
	rec := d.recordOf(sb)
	d.mu.Unlock()

	d.persistRec(ctx, rec)
	return nil
}

// SetHibernateTimeout changes a live sandbox's disk-pause timer: 0 = daemon
// default, negative = never. Persisted at once, like SetIdleTimeout.
func (d *Daemon) SetHibernateTimeout(ctx context.Context, id string, hibernate time.Duration) error {
	d.mu.Lock()
	sb, ok := d.boxes[id]
	if !ok {
		d.mu.Unlock()
		return rt.ErrNotFound
	}
	sb.HibernateTimeout = hibernate
	rec := d.recordOf(sb)
	d.mu.Unlock()

	d.persistRec(ctx, rec)
	return nil
}

// HibernateTimeoutDefault is the daemon-wide disk-pause timer; 0 means never.
func (d *Daemon) HibernateTimeoutDefault() time.Duration {
	if d == nil {
		return 0
	}
	return d.cfg.HibernateTimeout
}

// IdleTimeoutDefault is the daemon-wide idle standby timer; 0 means never.
func (d *Daemon) IdleTimeoutDefault() time.Duration {
	if d == nil {
		return 0
	}
	return d.cfg.IdleTimeout
}

// Refresh resets the hard TTL to HardTTL from now. No-op on a persistent
// sandbox, which would otherwise be demoted to a TTL sandbox.
func (d *Daemon) Refresh(ctx context.Context, id string) error {
	d.mu.Lock()
	sb, ok := d.boxes[id]
	if !ok {
		d.mu.Unlock()
		return rt.ErrNotFound
	}
	// Refresh is the documented keep-alive for a job that runs without API
	// traffic, so it resets the idle clock too — otherwise a sandbox that
	// faithfully refreshes its TTL is still frozen and then written to disk for
	// looking idle. Before the ExpiresAt check: a persistent sandbox has no TTL
	// to re-stamp but the same idle timers.
	sb.LastActiveAt = d.clock.Now()
	if sb.ExpiresAt.IsZero() {
		rec := d.recordOf(sb)
		d.mu.Unlock()
		d.persistRec(ctx, rec)
		return nil
	}
	sb.ExpiresAt = d.clock.Now().Add(d.cfg.HardTTL)
	rec := d.recordOf(sb)
	d.mu.Unlock()

	d.persistRec(ctx, rec)
	return nil
}

// templateUsers is the optional store lookup that finds every stored record
// on a template, loaded on this node or not.
type templateUsers interface {
	SandboxIDsOnTemplate(ctx context.Context, template string) ([]string, error)
}

// holdTemplate registers a start on tmpl until the returned release: a capture
// is never removed while something is booting from it and not yet in d.boxes.
func (d *Daemon) holdTemplate(tmpl string) (release func()) {
	d.captureMu.Lock()
	d.starting[tmpl]++
	d.captureMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			d.captureMu.Lock()
			if d.starting[tmpl]--; d.starting[tmpl] <= 0 {
				delete(d.starting, tmpl)
			}
			d.captureMu.Unlock()
		})
	}
}

// removeCaptureIfUnused removes tmpl, a rehome's or a move's capture, once no
// sandbox uses it: none loaded here in any state, none starting on it, no
// stored record but the one being destroyed (except), and no restore still
// loading records. A fork runs on its parent's capture, so each destroy checks
// again. A named snapshot is the snapshots API's to delete. captureMu is held
// from the check through the removal, so no start can begin using tmpl between.
// ponytail: one node-wide lock, held through a store read and a RemoveAll;
// per-template locks if destroys ever queue behind it.
func (d *Daemon) removeCaptureIfUnused(ctx context.Context, orgID, tmpl, except string) {
	if templates.Dir() == "" || !captureTemplate(orgID, tmpl) {
		return
	}
	d.captureMu.Lock()
	defer d.captureMu.Unlock()
	if d.starting[tmpl] > 0 || d.templateInUse(tmpl) {
		return
	}
	if u, ok := d.cfg.Store.(templateUsers); ok {
		ids, err := u.SandboxIDsOnTemplate(ctx, tmpl)
		if err != nil {
			d.log().Warn("capture template kept: its users could not be read", "template", tmpl, "error", err)
			return
		}
		if slices.ContainsFunc(ids, func(id string) bool { return id != except }) {
			return
		}
	}
	if err := os.RemoveAll(d.snapshotDir(tmpl)); err != nil {
		d.log().Warn("capture template remove failed", "template", tmpl, "error", err)
	}
}

// templateInUse reports a sandbox on this node, in any state, on tmpl, or a
// restore that has not loaded every record yet.
func (d *Daemon) templateInUse(tmpl string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.gone != nil {
		return true
	}
	for _, sb := range d.boxes {
		if sb.Template == tmpl {
			return true
		}
	}
	return false
}

// captureTemplate reports a rehome's or a move's capture of some sandbox of orgID.
func captureTemplate(orgID, tmpl string) bool {
	return strings.HasPrefix(tmpl, rehomePrefix) ||
		strings.HasPrefix(tmpl, store.SnapshotTemplateID(orgID, store.MoveSnapshotName("")))
}
