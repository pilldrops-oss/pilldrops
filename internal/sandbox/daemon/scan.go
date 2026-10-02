package daemon

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// recordEvent appends a lifecycle event with the org's current running count.
// Best-effort; call after the state transition so the count is accurate. Takes d.mu itself.
func (d *Daemon) recordEvent(ctx context.Context, orgID, sandboxID, event, template string) {
	if d.cfg.Store == nil {
		return
	}
	d.mu.RLock()
	n := 0
	for _, sb := range d.boxes {
		if sb.OrgID == orgID && sb.State == StateRunning {
			n++
		}
	}
	d.mu.RUnlock()
	if err := d.cfg.Store.RecordEvent(ctx, orgID, sandboxID, event, template, n, d.clock.Now().Unix()); err != nil {
		d.log().Warn("record event failed", "event", event, "sandbox_id", sandboxID, "error", err)
	}
}

// scanOOMKills records an "oom" event whenever a running sandbox's cumulative
// OOM-kill count rises. Runtimes without rt.OOMReporter are skipped.
func (d *Daemon) scanOOMKills(ctx context.Context) {
	type target struct {
		id, org, tmpl string
		inst          rt.Instance
		seen          int
	}
	var targets []target
	d.mu.RLock()
	for _, sb := range d.boxes {
		if sb.State == StateRunning && sb.inst != nil {
			targets = append(targets, target{sb.ID, sb.OrgID, sb.Template, sb.inst, sb.oomSeen})
		}
	}
	d.mu.RUnlock()

	forEachTarget(ctx, len(targets), scanConcurrency, func(i int) {
		t := targets[i]
		r, ok := t.inst.(rt.OOMReporter)
		if !ok {
			return
		}
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		count, err := r.OOMKillCount(cctx)
		cancel()
		if err != nil || count <= t.seen {
			return
		}
		// Record outside the lock, then take it only to advance the watermark.
		d.recordEvent(ctx, t.org, t.id, "oom", t.tmpl)
		d.log().Warn("oom detected", "sandbox_id", t.id, "org_id", t.org, "kills", count, "previous_kills", t.seen)
		d.mu.Lock()
		if sb := d.boxes[t.id]; sb != nil {
			sb.oomSeen = count
		}
		d.mu.Unlock()
	})
}

// scanExits reaps a running sandbox whose VMM or container died underneath it,
// and says why: the event and the history's exit state are "oom" when the
// runtime saw a memory kill, else "exited". Probed here, off the reconcile
// lock, because a container's liveness is an Engine API round trip.
func (d *Daemon) scanExits(ctx context.Context) {
	type target struct {
		id string
		lv rt.Liveness
	}
	var targets []target
	d.mu.RLock()
	for _, sb := range d.boxes {
		if lv, ok := sb.inst.(rt.Liveness); ok && sb.State == StateRunning {
			targets = append(targets, target{sb.ID, lv})
		}
	}
	d.mu.RUnlock()

	forEachTarget(ctx, len(targets), scanConcurrency, func(i int) {
		t := targets[i]
		if t.lv.Alive() {
			return
		}
		if rv, ok := t.lv.(rt.Reviver); ok && rv.Casualty() {
			d.reviveCasualty(ctx, t.id, t.lv, rv)
			return
		}
		reason := "exited"
		if r, ok := t.lv.(rt.OOMKilledReporter); ok && r.OOMKilled() {
			reason = "oom"
		}
		switch err := d.destroy(ctx, t.id, false, reason, t.lv); {
		case err == nil:
			d.log().Warn("sandbox instance died underneath it", "sandbox_id", t.id, "reason", reason)
		case !errors.Is(err, errNoLongerDead) && !errors.Is(err, rt.ErrNotFound):
			d.log().Warn("reaping a dead sandbox failed", "sandbox_id", t.id, "error", err)
		}
	})
}

// reviveCasualty starts again a sandbox its engine's restart stopped, rather
// than reaping it. One that will not start is marked failed with an "exited"
// event and kept, data and all: only a destroy removes it.
func (d *Daemon) reviveCasualty(ctx context.Context, id string, was rt.Liveness, rv rt.Reviver) {
	sb, release, err := d.acquireOp(id)
	if err != nil {
		return
	}
	defer release()
	d.mu.Lock()
	lv, _ := sb.inst.(rt.Liveness)
	current := lv == was && sb.State == StateRunning
	orgID, template, startCmd, env := sb.OrgID, sb.Template, sb.StartCmd, sb.Env
	d.mu.Unlock()
	if !current {
		return
	}

	inst, err := rv.Revive(ctx)
	d.mu.Lock()
	if err != nil {
		sb.State = StateFailed
	} else {
		sb.inst = inst
	}
	rec := d.recordOf(sb)
	d.mu.Unlock()
	d.persistRec(ctx, rec)
	if err != nil {
		d.recordEvent(ctx, orgID, id, "exited", template)
		d.log().Error("sandbox stopped by its engine restarting would not start again; marked failed, data kept",
			"sandbox_id", id, "error", err)
		return
	}
	// The container's own command came back with it; a registered server did not.
	if startCmd != "" {
		if err := inst.StartServer(ctx, startCmd, env); err != nil {
			d.log().Warn("revive: app restart failed", "sandbox_id", id, "error", err)
		}
	}
	d.recordEvent(ctx, orgID, id, "restarted", template)
	d.log().Warn("sandbox stopped by its engine restarting was started again", "sandbox_id", id)
}

// scanConcurrency bounds how many sandboxes a scanner probes at once.
const scanConcurrency = 16

// forEachTarget runs fn for indices [0,n), at most limit at a time, stopping
// when ctx is done. The limit is the caller's: a scan probe and a lifecycle op
// cost very different amounts.
func forEachTarget(ctx context.Context, n, limit int, fn func(i int)) {
	if n == 0 {
		return
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			fn(i)
		}()
	}
	wg.Wait()
}

// scanServerCrashes emits a "crash" event on the up-to-down edge of a
// sandbox's registered server, so a slow-to-bind server is not a false crash.
func (d *Daemon) scanServerCrashes(ctx context.Context) {
	type target struct {
		id, org, tmpl string
		inst          rt.Instance
		port          int
		wasUp         bool
	}
	var targets []target
	d.mu.RLock()
	for _, sb := range d.boxes {
		if sb.State == StateRunning && sb.inst != nil && sb.StartCmd != "" && sb.PublicPort != 0 {
			targets = append(targets, target{sb.ID, sb.OrgID, sb.Template, sb.inst, sb.PublicPort, sb.serverUp})
		}
	}
	d.mu.RUnlock()

	forEachTarget(ctx, len(targets), scanConcurrency, func(i int) {
		t := targets[i]
		addr, err := t.inst.ListenTarget(t.port)
		up := err == nil && addr != "" && dialable(ctx, addr)
		if up == t.wasUp {
			return
		}
		d.mu.Lock()
		if sb := d.boxes[t.id]; sb != nil {
			sb.serverUp = up
		}
		d.mu.Unlock()
		if !up {
			d.recordEvent(ctx, t.org, t.id, "crash", t.tmpl)
			d.log().Warn("registered server stopped answering", "sandbox_id", t.id, "org_id", t.org, "port", t.port)
		}
	})
}

// dialable reports whether a TCP address accepts a connection right now.
func dialable(ctx context.Context, addr string) bool {
	d := &net.Dialer{Timeout: 2 * time.Second}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}
