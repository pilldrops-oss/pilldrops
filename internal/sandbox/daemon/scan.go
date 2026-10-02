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
		if ctx.Err() == nil {
			d.noteReach(ctx, t.id, t.org, t.tmpl, t.inst, err == nil)
		}
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

// unreachableAfter is how many scans in a row must fail to exec into a running
// sandbox before it is reported. Under firecracker every exec rides envd, and
// the guest's reaper stops restarting envd after ten quick exits: without this
// the sandbox stayed "running" while every exec answered 503.
const unreachableAfter = 3

// noteReach counts the OOM probe's failed execs into inst and records an
// "unreachable" event once, on the scan that reaches unreachableAfter. The count
// is per instance: a pause, resume or revive starts it again, and a probe of an
// instance the sandbox no longer runs on counts for nothing.
func (d *Daemon) noteReach(ctx context.Context, id, org, tmpl string, inst rt.Instance, ok bool) {
	d.mu.Lock()
	sb := d.boxes[id]
	if sb == nil || sb.inst != inst || sb.State != StateRunning {
		d.mu.Unlock()
		return
	}
	if sb.probeInst != inst {
		sb.probeInst, sb.probeFails = inst, 0
	}
	if ok {
		sb.probeFails = 0
	} else {
		sb.probeFails++
	}
	report := sb.probeFails == unreachableAfter
	d.mu.Unlock()
	if report {
		d.recordEvent(ctx, org, id, "unreachable", tmpl)
		d.log().Error("cannot exec into a running sandbox; its envd is down", "sandbox_id", id, "org_id", org)
	}
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
// than reaping it. One confirmed stopped again right after its start
// (rt.ErrWontStayUp), or dead again after maxRevives confirmed revives under
// the same engine start, is a crash: destroyed with an "exited" event. Any other revive error (dockerd still settling) is retried with
// backoff and never counted.
func (d *Daemon) reviveCasualty(ctx context.Context, id string, was rt.Liveness, rv rt.Reviver) {
	// Its own deadline: a docker start with retries plus the app's readiness
	// wait outlasts the scan and restore budgets its callers run on.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reviveTimeout)
	defer cancel()
	go func() {
		select {
		case <-d.reviveStop:
			cancel()
		case <-ctx.Done():
		}
	}()
	sb, release, err := d.acquireOp(id)
	if err != nil {
		return
	}
	d.mu.Lock()
	lv, _ := sb.inst.(rt.Liveness)
	current := lv == was && sb.State == StateRunning
	orgID, template, startCmd, env := sb.OrgID, sb.Template, sb.StartCmd, sb.Env
	if !current {
		d.mu.Unlock()
		release()
		return
	}
	now := d.clock.Now()
	if now.Before(sb.reviveRetryAt) {
		d.mu.Unlock()
		release()
		return
	}
	engine := rv.EngineStarted()
	if !engine.Equal(sb.reviveEngine) {
		sb.revives = 0
	}
	// Dead again under the engine of its maxRevives-th confirmed revive.
	exhausted := sb.revives >= maxRevives
	d.mu.Unlock()

	if exhausted {
		err = errors.New("died again after each of its last revives")
	} else {
		var inst rt.Instance
		inst, err = rv.Revive(ctx)
		if err != nil && closed(d.reviveStop) {
			// Not a container that will not start: the daemon is stopping. Left
			// as it was, so the next start adopts and starts it again.
			release()
			d.log().Info("revive cancelled by the daemon stopping", "sandbox_id", id)
			return
		}
		if err != nil && !errors.Is(err, rt.ErrWontStayUp) {
			// Nothing read it stopped after a start: the engine is still
			// settling. Not counted, never a crash; retried with backoff, and
			// not metered as running while it is down.
			d.mu.Lock()
			sb.reviveFails++
			wait := reviveBackoff(sb.reviveFails)
			sb.reviveRetryAt = now.Add(wait)
			var u Usage
			if sb.meter.running {
				u = sb.meter.sample(now)
				sb.meter.freeze(now)
			}
			d.mu.Unlock()
			release()
			d.flushUsage(ctx, id, orgID, u)
			d.log().Warn("revive failed; retrying", "sandbox_id", id, "in", wait, "error", err)
			return
		}
		if err == nil {
			d.mu.Lock()
			sb.inst = inst
			sb.revives++
			sb.reviveEngine = engine
			sb.reviveFails, sb.reviveRetryAt = 0, time.Time{}
			if !sb.meter.running {
				sb.meter.resume(d.clock.Now())
			}
			rec := d.recordOf(sb)
			d.mu.Unlock()
			d.persistRec(ctx, rec)
			// The container's own command came back with it; a registered server did not.
			if startCmd != "" {
				if err := inst.StartServer(ctx, startCmd, env); err != nil {
					d.log().Warn("revive: app restart failed", "sandbox_id", id, "error", err)
				}
			}
			release()
			d.recordEvent(ctx, orgID, id, "restarted", template)
			d.log().Warn("sandbox stopped by its engine restarting was started again", "sandbox_id", id)
			return
		}
	}
	release()
	d.log().Error("sandbox would not stay up after restart; destroying it as a crash",
		"sandbox_id", id, "error", err)
	// was, not nil: destroy re-checks under the op lock that the sandbox still
	// runs that instance and it still reads stopped.
	if err := d.destroy(ctx, id, false, "exited", was); err != nil &&
		!errors.Is(err, rt.ErrNotFound) && !errors.Is(err, errNoLongerDead) {
		d.log().Warn("destroying a sandbox that would not stay up failed", "sandbox_id", id, "error", err)
	}
}

// maxRevives bounds casualty revives under one engine start: a container that
// starts and dies again is a crash, not a casualty.
const maxRevives = 3

// reviveBackoffBase and reviveBackoffMax space the retries of a revive whose
// start failed without a confirmed stop: doubling from one scan's worth.
const (
	reviveBackoffBase = 8 * time.Second
	reviveBackoffMax  = 2 * time.Minute
)

// reviveBackoff is the wait before the n-th retry (n >= 1). The shift is capped
// so a long outage stays at reviveBackoffMax instead of overflowing to zero.
func reviveBackoff(n int) time.Duration {
	return min(reviveBackoffBase<<min(n-1, 8), reviveBackoffMax)
}

// reviveTimeout bounds one revive, start retries and app restart included.
const reviveTimeout = 2 * time.Minute

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
