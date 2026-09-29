package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// wakeTimeout bounds a wake started by a call that carries no context of its own.
const wakeTimeout = 90 * time.Second

// ensureRunning makes a paused or standby sandbox running before a call that
// needs a live instance: it resumes or thaws it and waits out an in-flight
// pause or resume, bounded by ctx. Resume is idempotent and serialized on opMu, so concurrent
// wakers collapse onto one restore.
func (d *Daemon) ensureRunning(ctx context.Context, id string) error {
	d.mu.RLock()
	sb, ok := d.boxes[id]
	var state State
	if ok {
		state = sb.State
	}
	d.mu.RUnlock()
	if !ok {
		return rt.ErrNotFound
	}
	switch state {
	case StateRunning:
		return nil
	case StatePaused, StateStandby, StatePausing, StateResuming:
	default:
		return fmt.Errorf("sandbox is %s", state)
	}
	// The restore itself must finish even if the caller gives up, or the
	// sandbox is left half-resumed; only the wait is bounded.
	done := make(chan error, 1)
	go func() { done <- d.Resume(context.WithoutCancel(ctx), id) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// wakeCtx is the bound for wakes from calls without a caller context.
func wakeCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), wakeTimeout)
}

// wakeAndUse wakes a sandbox and hands fn its live instance. d.mu is held for
// read across fn, which is what makes the instance safe to touch -- so fn must
// not block. Callers that need the sandbox record, or that must not hold the
// lock, do the dance themselves.
func wakeAndUse[T any](d *Daemon, id string, fn func(rt.Instance) (T, error)) (T, error) {
	var zero T
	ctx, cancel := wakeCtx()
	defer cancel()
	if err := d.ensureRunning(ctx, id); err != nil {
		return zero, err
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	sb, ok := d.boxes[id]
	if !ok {
		return zero, rt.ErrNotFound
	}
	if sb.inst == nil {
		return zero, rt.ErrPaused
	}
	return fn(sb.inst)
}

// RuntimeName reports which runtime this node boots sandboxes with.
func (d *Daemon) RuntimeName() string {
	if d == nil || d.cfg.Runtime == nil {
		return ""
	}
	return d.cfg.Runtime.Name()
}

// DiskPressure reports whether the runtime is already refusing work because
// its filesystem is too full, for callers that would otherwise start a large
// write before Create's own guard is reached. False when the runtime does not
// track it, so nothing is refused on a runtime with no such notion.
func (d *Daemon) DiskPressure() bool {
	if d == nil || d.cfg.Runtime == nil {
		return false
	}
	p, ok := d.cfg.Runtime.(rt.DiskPressured)
	return ok && p.DiskPressured()
}

// RuntimeCaps reports the runtime's capabilities. ok is false when no runtime
// is configured; that must not read as a zero Caps, which would claim no isolation.
func (d *Daemon) RuntimeCaps() (rt.Caps, bool) {
	if d == nil || d.cfg.Runtime == nil {
		return rt.Caps{}, false
	}
	return d.cfg.Runtime.Capabilities(), true
}

// StorageCapUnenforced reports how many sandboxes booted without an enforced
// disk cap, or 0 when the runtime always enforces one or cannot report.
func (d *Daemon) StorageCapUnenforced() int64 {
	if d.cfg.Runtime == nil {
		return 0
	}
	if r, ok := d.cfg.Runtime.(rt.StorageCapReporter); ok {
		return r.StorageCapUnenforced()
	}
	return 0
}

// FS returns the filesystem of a sandbox, waking it if paused.
func (d *Daemon) FS(id string) (rt.FileSystem, error) {
	return wakeAndUse(d, id, func(inst rt.Instance) (rt.FileSystem, error) { return inst.FS(), nil })
}

// SetEnv pushes a config env change into a runtime that can update the guest's
// session environment (rt.EnvSetter). Others return rt.ErrUnsupported and keep
// the per-exec merge.
func (d *Daemon) SetEnv(ctx context.Context, id string, env map[string]string) error {
	d.mu.RLock()
	sb, ok := d.boxes[id]
	d.mu.RUnlock()
	if !ok {
		return rt.ErrNotFound
	}
	if sb.inst == nil {
		return rt.ErrPaused
	}
	es, ok := sb.inst.(rt.EnvSetter)
	if !ok {
		return rt.ErrUnsupported
	}
	return es.SetEnv(ctx, env)
}

// AgentEndpoint reports where the sandbox's agentd listens and its token.
// Returns rt.ErrUnsupported when the runtime has no agent surface. Does not
// touch LastActiveAt, so lookups do not keep an idle machine awake.
func (d *Daemon) AgentEndpoint(id string) (addr string, token string, err error) {
	ctx, cancel := wakeCtx()
	defer cancel()
	if err := d.ensureRunning(ctx, id); err != nil {
		return "", "", err
	}
	d.mu.Lock()
	sb, ok := d.boxes[id]
	if !ok {
		d.mu.Unlock()
		return "", "", rt.ErrNotFound
	}
	state, inst := sb.State, sb.inst
	d.mu.Unlock()

	if state == StatePaused {
		return "", "", rt.ErrPaused
	}
	t, ok := inst.(rt.AgentTargeter)
	if !ok {
		return "", "", rt.ErrUnsupported
	}
	return t.AgentTarget()
}

// Exec runs a command in a sandbox, waking it if paused.
func (d *Daemon) Exec(ctx context.Context, id string, s rt.ExecSpec) (rt.ExecResult, error) {
	if err := d.ensureRunning(ctx, id); err != nil {
		return rt.ExecResult{}, err
	}
	inst, err := d.execInstance(id)
	if err != nil {
		return rt.ExecResult{}, err
	}
	return inst.Exec(ctx, s)
}

// execInstance is the gate every command-running call passes: the sandbox
// exists, its state allows a command, and the idle clock is stamped because a
// command is activity. The lock is dropped before the returned instance is
// used -- an exec can take as long as the command does.
func (d *Daemon) execInstance(id string) (rt.Instance, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	sb, ok := d.boxes[id]
	if !ok {
		return nil, rt.ErrNotFound
	}
	if !canTransition(sb.State, "exec") {
		return nil, rt.ErrPaused
	}
	sb.LastActiveAt = d.clock.Now()
	return sb.inst, nil
}

// ExecStream runs a command and delivers output frames via onFrame. Runtimes
// without rt.StreamExecutor fall back to a buffered exec emitted as frames.
func (d *Daemon) ExecStream(ctx context.Context, id string, s rt.ExecSpec, onFrame func(rt.StreamFrame)) error {
	if err := d.ensureRunning(ctx, id); err != nil {
		return err
	}
	inst, err := d.execInstance(id)
	if err != nil {
		return err
	}
	if se, ok := inst.(rt.StreamExecutor); ok {
		return se.ExecStream(ctx, s, onFrame)
	}
	res, err := inst.Exec(ctx, s)
	if err != nil {
		return err
	}
	if len(res.Stdout) > 0 {
		onFrame(rt.StreamFrame{Stdout: []byte(res.Stdout)})
	}
	if len(res.Stderr) > 0 {
		onFrame(rt.StreamFrame{Stderr: []byte(res.Stderr)})
	}
	onFrame(rt.StreamFrame{ExitCode: res.ExitCode, Exited: true, Truncated: res.Truncated})
	return nil
}

// Procs lists processes in a sandbox, waking it if paused.
func (d *Daemon) Procs(id string) ([]rt.Proc, error) {
	return wakeAndUse(d, id, func(inst rt.Instance) ([]rt.Proc, error) { return inst.Procs() })
}

// LiveStats samples CPU/mem/disk for a running sandbox. ok is false when the
// sandbox is unknown, paused, or its runtime cannot sample. The runtime call
// can take seconds, so it runs outside the daemon lock.
func (d *Daemon) LiveStats(ctx context.Context, id string) (rt.LiveStats, bool) {
	d.mu.RLock()
	sb, ok := d.boxes[id]
	var inst rt.Instance
	if ok {
		inst = sb.inst
	}
	d.mu.RUnlock()
	if !ok || inst == nil {
		return rt.LiveStats{}, false
	}
	ls, can := inst.(rt.LiveStatser)
	if !can {
		return rt.LiveStats{}, false
	}
	return d.liveStats.Do(ctx, id, d.clock.Now(), func(ctx context.Context) (rt.LiveStats, bool) {
		stats, err := ls.LiveStats(ctx)
		if err != nil {
			return rt.LiveStats{}, false
		}
		return stats, true
	})
}

// Signal sends signal sig to pid in a sandbox, waking it if paused.
func (d *Daemon) Signal(id string, pid, sig int) error {
	_, err := wakeAndUse(d, id, func(inst rt.Instance) (struct{}, error) {
		return struct{}{}, inst.Signal(pid, sig)
	})
	return err
}

// Rename sets a sandbox's label (empty clears it) and persists it immediately.
// Validation is the caller's job.
func (d *Daemon) Rename(ctx context.Context, id, name string) error {
	d.mu.Lock()
	sb, ok := d.boxes[id]
	if !ok {
		d.mu.Unlock()
		return rt.ErrNotFound
	}
	sb.Name = name
	rec := d.recordOf(sb)
	d.mu.Unlock()
	d.persistRec(ctx, rec)
	return nil
}

// SetVisibility records "org" or "private" on a sandbox, in memory and in the
// store. The record is authoritative, so a paused sandbox flips without being
// woken; the daemon enforces nothing, the HTTP layer reads it.
func (d *Daemon) SetVisibility(ctx context.Context, id, visibility string) error {
	d.mu.Lock()
	sb, ok := d.boxes[id]
	if !ok {
		d.mu.Unlock()
		return rt.ErrNotFound
	}
	sb.Visibility = visibility
	rec := d.recordOf(sb)
	d.mu.Unlock()
	d.persistRec(ctx, rec)
	return nil
}

// ErrSlugTaken is returned by SetSlug when another live sandbox holds the slug.
var ErrSlugTaken = errors.New("slug is taken")

// SetSlug claims (or with "" releases) the sandbox's slug. The store's unique
// index is the arbiter across nodes, so the save is synchronous and a
// conflict rolls the in-memory value back. Without a store the node alone
// arbitrates.
func (d *Daemon) SetSlug(ctx context.Context, id, slug string) error {
	d.mu.Lock()
	sb, ok := d.boxes[id]
	if !ok {
		d.mu.Unlock()
		return rt.ErrNotFound
	}
	if slug != "" {
		for oid, o := range d.boxes {
			if oid != id && o.Slug == slug {
				d.mu.Unlock()
				return fmt.Errorf("%w: %q", ErrSlugTaken, slug)
			}
		}
	}
	prev := sb.Slug
	sb.Slug = slug
	rec := d.recordOf(sb)
	d.mu.Unlock()
	if d.cfg.Store == nil {
		return nil
	}
	if err := d.cfg.Store.SaveSandbox(ctx, rec); err != nil {
		d.mu.Lock()
		if sb.Slug == slug {
			sb.Slug = prev
		}
		d.mu.Unlock()
		if errors.Is(err, store.ErrSlugTaken) {
			return fmt.Errorf("%w: %q", ErrSlugTaken, slug)
		}
		return err
	}
	// Peers resolve this sandbox by its slug, so the rename has to reach the
	// resolver too or <old-slug>.pd keeps answering.
	if err := d.pushNetworks(ctx, id); err != nil {
		d.log().Warn("rename: could not refresh network membership", "sandbox_id", id, "error", err)
	}
	return nil
}

// BySlug resolves a slug to the sandbox holding it on this node and its public
// port (8000 when none was declared). ponytail: O(n) scan under the read lock;
// index it if a node ever holds thousands of slugged sandboxes.
func (d *Daemon) BySlug(_ context.Context, slug string) (string, int, error) {
	if slug == "" {
		return "", 0, rt.ErrNotFound
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	for id, sb := range d.boxes {
		if sb.Slug == slug {
			port := sb.PublicPort
			if port == 0 {
				port = 8000
			}
			return id, port, nil
		}
	}
	return "", 0, rt.ErrNotFound
}

// Touch resets the idle timer.
func (d *Daemon) Touch(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if sb, ok := d.boxes[id]; ok {
		sb.LastActiveAt = d.clock.Now()
	}
}
