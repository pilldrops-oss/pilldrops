package daemon

import (
	"context"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// reconcileBudget bounds one reconcile tick; a tick that overruns is abandoned
// and the next one retries.
const reconcileBudget = 30 * time.Second

// lifecycleConcurrency caps concurrent pauses/destroys per tick. opMu already
// serializes per sandbox; the cap is about disk I/O.
const lifecycleConcurrency = 8

// Start restores persisted sandboxes, then runs the reconciler until Stop.
func (d *Daemon) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	d.bgCancel = cancel

	restoreCtx, restoreCancel := context.WithTimeout(ctx, restoreBudget)
	restored := d.restoreFromStore(restoreCtx)
	restoreCancel()
	// After the store is read, so a checkpoint is only swept when its sandbox is
	// genuinely gone rather than merely not restored yet.
	var retry restoreRetry
	if restored {
		d.sweepCheckpoints()
		d.resumeAfterRestart()
	} else {
		retry.failed(d.clock.Now())
	}

	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				tickCtx, tickCancel := context.WithTimeout(ctx, reconcileBudget)
				if !restored && retry.due(d.clock.Now()) {
					if restored = d.restoreFromStore(tickCtx); restored {
						d.sweepCheckpoints()
						d.resumeAfterRestart()
					} else {
						retry.failed(d.clock.Now())
					}
				}
				start := d.clock.Now()
				d.reconcileOnce(tickCtx)
				d.metrics.ReconcileDuration.Observe(d.clock.Now().Sub(start))
				if tickCtx.Err() != nil {
					// Counted, not logged: under sustained overload this fires every second.
					d.metrics.ReconcileOverruns.Inc()
				}
				tickCancel()
			case <-ctx.Done():
				return
			}
		}
	}()
	// Expired backups are reclaimed on their own slow loop: it is two deletes
	// in two systems, and it must not share a budget with the scanners.
	go func() {
		ticker := time.NewTicker(backupPruneInterval)
		defer ticker.Stop()
		for {
			if _, err := d.PruneBackups(ctx, BackupRetainDays()); err != nil {
				d.log().Warn("backup retention sweep failed", "error", err)
			}
			select {
			case <-ticker.C:
			case <-ctx.Done():
				return
			}
		}
	}()
	// The scanners exec into each running sandbox, so they run on a slower loop.
	go func() {
		ticker := time.NewTicker(8 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				start := d.clock.Now()
				d.scanOnce(ctx)
				d.metrics.ScanDuration.Observe(d.clock.Now().Sub(start))
			case <-ctx.Done():
				return
			}
		}
	}()
}

// scanOnce is one scanner pass. The exit scan has a budget of its own and goes
// first: behind the probes into every guest, a slow fleet starved it, and a dead
// VMM's netns, chroot and overlay leak until it is reaped.
func (d *Daemon) scanOnce(ctx context.Context) {
	exitCtx, exitCancel := context.WithTimeout(ctx, scanBudget)
	d.scanExits(exitCtx)
	exitCancel()

	scanCtx, scanCancel := context.WithTimeout(ctx, scanBudget)
	defer scanCancel()
	d.scanOOMKills(scanCtx)
	d.scanServerCrashes(scanCtx)
}

// scanBudget bounds each scanner pass; a var so tests can shrink it. A slow
// pass delays the next tick rather than overlapping it.
var scanBudget = 7 * time.Second

// Stop halts the reconciler. Idempotent.
func (d *Daemon) Stop() {
	d.stopOnce.Do(func() {
		close(d.stop)
		if d.bgCancel != nil {
			d.bgCancel()
		}
	})
	d.stopRevives()
}

// reconcileOnce is one tick of the control loop, extracted so tests can drive
// it with a manual clock. Store I/O and lifecycle calls happen after the lock
// is released.
func (d *Daemon) reconcileOnce(ctx context.Context) {
	now := d.clock.Now()

	var deltas []usageDelta
	var toPause, toStandby, toStop []string

	// Usage is flushed on a slow cadence; Pause and Destroy flush explicitly.
	flushDue := now.Sub(d.lastUsageFlush) >= usageFlushInterval

	// The two idle timers have different requirements: standby needs an instance
	// that can freeze, hibernate needs a runtime that can snapshot to disk.
	// A runtime with neither runs its sandboxes to their hard TTL.
	caps := rt.Caps{}
	if d.cfg.Runtime != nil {
		caps = d.cfg.Runtime.Capabilities()
	}

	d.mu.Lock()
	for id, sb := range d.boxes {
		// Meter paused sandboxes too: storage keeps accruing while paused.
		if flushDue {
			if u := sb.meter.sample(now); !u.IsZero() {
				deltas = append(deltas, usageDelta{sb.ID, sb.OrgID, u})
			}
		}
		// A zero ExpiresAt means persistent, exempt from the TTL reaper.
		if !sb.ExpiresAt.IsZero() && now.After(sb.ExpiresAt) {
			toStop = append(toStop, id)
			continue
		}
		// Abandonment is a persistent sandbox's only bound. LastActiveAt stops
		// advancing while paused, so this measures time since last resume.
		if sb.ExpiresAt.IsZero() && d.cfg.AbandonAfter > 0 &&
			now.Sub(sb.LastActiveAt) >= d.cfg.AbandonAfter {
			d.log().Info("abandoning sandbox", "sandbox_id", id, "idle", now.Sub(sb.LastActiveAt).Round(time.Hour).String())
			toStop = append(toStop, id)
			continue
		}
		// Two timers off the same LastActiveAt clock, both with the per-sandbox
		// override shape (0 = daemon default; <0 = never). The hibernate one is
		// checked first: once a sandbox is that idle, the disk tier is where it
		// belongs whether or not it went through standby on the way.
		idle := d.cfg.IdleTimeout
		if sb.IdleTimeout != 0 {
			idle = sb.IdleTimeout
		}
		hibernate := d.cfg.HibernateTimeout
		if sb.HibernateTimeout != 0 {
			hibernate = sb.HibernateTimeout
		}
		switch {
		case caps.Snapshot && hibernate > 0 && now.Sub(sb.LastActiveAt) >= hibernate &&
			(sb.State == StateRunning || sb.State == StateStandby):
			toPause = append(toPause, id)
		case caps.Standby && idle > 0 && sb.State == StateRunning &&
			now.Sub(sb.LastActiveAt) >= idle:
			toStandby = append(toStandby, id)
		}
	}
	if flushDue {
		d.lastUsageFlush = now
	}
	d.mu.Unlock()

	d.flushUsageBatch(ctx, deltas)
	d.runLifecycle(ctx, toStandby, "standby", func(ctx context.Context, id string) error {
		_, err := d.Standby(ctx, id)
		return err
	})
	d.runLifecycle(ctx, toPause, "pause", d.Pause)
	d.runLifecycle(ctx, toStop, "stop", d.Destroy)
}

// runLifecycle applies op to each id with bounded concurrency; one failure never abandons the rest.
func (d *Daemon) runLifecycle(ctx context.Context, ids []string, what string, op func(context.Context, string) error) {
	if len(ids) == 0 {
		return
	}
	// A cancelled ctx stops the fan-out; the next tick re-derives the same work.
	forEachTarget(ctx, len(ids), lifecycleConcurrency, func(i int) {
		if err := op(ctx, ids[i]); err != nil {
			d.log().Warn("reconcile lifecycle op failed", "op", what, "sandbox_id", ids[i], "error", err)
		}
	})
}

type usageDelta struct {
	id, orgID string
	u         Usage
}

// usageFlushInterval is how often metered usage reaches the store; a hard kill
// loses at most one interval.
const usageFlushInterval = time.Minute

// flushUsageBatch writes metered deltas, batched when the store supports it.
func (d *Daemon) flushUsageBatch(ctx context.Context, deltas []usageDelta) {
	if len(deltas) == 0 || d.cfg.Store == nil {
		return
	}
	if bw, ok := d.cfg.Store.(usageBatchWriter); ok {
		recs := make([]UsageRecord, 0, len(deltas))
		for _, u := range deltas {
			recs = append(recs, UsageRecord{SandboxID: u.id, OrgID: u.orgID, Usage: u.u})
		}
		if err := bw.RecordUsageBatch(ctx, recs); err == nil {
			return
		} else {
			d.log().Warn("batch usage flush failed, falling back to per-sandbox writes", "error", err)
		}
	}
	for _, u := range deltas {
		d.flushUsage(ctx, u.id, u.orgID, u.u)
	}
}

// UsageRecord is one sandbox's metered delta, for a batched flush.
type UsageRecord struct {
	SandboxID string
	OrgID     string
	Usage     Usage
}

// usageBatchWriter is an optional Persister capability.
type usageBatchWriter interface {
	RecordUsageBatch(ctx context.Context, recs []UsageRecord) error
}

// restoreBudget bounds reading persisted sandboxes back at boot; a var so
// tests can shrink it.
var restoreBudget = 60 * time.Second

// restoreRetryMax caps the wait between restore retries.
const restoreRetryMax = 30 * time.Second

// restoreRetry spaces out retries of an unfinished restore, doubling from a
// second to restoreRetryMax: with docker down for good, every tick reloaded
// the store and logged an error.
type restoreRetry struct {
	wait time.Duration
	next time.Time
}

func (b *restoreRetry) due(now time.Time) bool { return !now.Before(b.next) }

func (b *restoreRetry) failed(now time.Time) {
	b.wait = min(max(2*b.wait, time.Second), restoreRetryMax)
	b.next = now.Add(b.wait)
}
