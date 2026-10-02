package daemon

import (
	"context"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// restoreFromStore reloads persisted sandboxes after a restart. Running records
// whose instance still exists, live or stopped, are adopted; records with a
// snapshot come back paused; stopped, expired or unrestorable records this node
// owns are dropped. A record is never dropped while its instance exists.
func (d *Daemon) restoreFromStore(ctx context.Context) {
	if d.cfg.Store == nil {
		return
	}
	recs, err := d.cfg.Store.LoadSandboxes(ctx)
	if err != nil {
		d.log().Error("restore: load sandboxes failed", "error", err)
		return
	}
	adopter, _ := d.cfg.Runtime.(rt.Adopter)
	now := d.clock.Now()
	for _, r := range recs {
		// Only delete records this node provably owns: an unowned row on a
		// shared database is usually another node's live sandbox. An empty
		// Config.NodeID means unclustered, so every record is owned.
		owned := d.cfg.NodeID == "" || r.NodeID == d.cfg.NodeID
		drop := func(reason string) {
			if !owned {
				d.log().Info("restore: leaving unowned record alone",
					"sandbox_id", r.ID, "reason", reason, "owner", r.NodeID)
				return
			}
			d.log().Warn("restore: dropping record", "sandbox_id", r.ID, "reason", reason)
			if err := d.cfg.Store.DeleteSandbox(ctx, r.ID); err != nil {
				d.log().Warn("restore: record delete failed", "sandbox_id", r.ID, "error", err)
			}
		}
		switch {
		case r.State == string(StateStopped):
			drop("already stopped")
			continue
		// ExpiresAt == 0 means persistent; the guard keeps it from being dropped as expired.
		case r.ExpiresAt > 0 && now.Unix() >= r.ExpiresAt:
			// Stop a still-live instance before dropping so it is not leaked.
			if adopter != nil {
				if inst, ok := adopter.Adopt(context.Background(), r.ID); ok {
					if err := inst.Stop(context.Background()); err != nil {
						d.log().Warn("restore: expired instance would not stop", "sandbox_id", r.ID, "error", err)
					}
				}
			}
			drop("past hard TTL")
			continue
		}

		// A standby sandbox's instance is frozen, not stopped, so it outlives the
		// daemon exactly as a running one does and adopts the same way. It is
		// thawed rather than adopted frozen: nothing would ever unfreeze it,
		// since the state it came back in is "running".
		// "lost" is the control plane's mark on a node it reaped (store.StateLost).
		// This node is back, so a VM that survived is adopted, not swept.
		// "failed" is a sandbox whose instance would not start again; its
		// container still holds its data, so it is adopted and retried, never swept.
		if (r.State == string(StateRunning) || r.State == string(StateStandby) ||
			r.State == "lost" || r.State == string(StateFailed)) && adopter != nil {
			if inst, ok := adopter.Adopt(context.Background(), r.ID); ok {
				// A stopped instance (a host reboot) is started again the way the
				// exit scan starts one a dockerd restart stopped.
				rv, stopped := inst.(rt.Reviver)
				stopped = stopped && rv.Casualty()
				if fr, isFreezer := inst.(rt.Freezer); isFreezer && r.State == string(StateStandby) && !stopped {
					if err := fr.Thaw(context.Background()); err != nil {
						d.log().Warn("restore: thaw of an adopted standby sandbox failed",
							"sandbox_id", r.ID, "error", err)
					}
				}
				d.adoptRunning(ctx, r, inst, now)
				if lv, ok := inst.(rt.Liveness); ok && stopped {
					d.reviveCasualty(ctx, r.ID, lv, rv)
				}
				continue
			}
		}

		if r.Snapshot == "" {
			drop("no snapshot to restore from, no live instance to adopt")
			continue
		}
		// Advisory: a missing snapshot must not drop the record, since a slow
		// network mount at boot looks identical. A later failed resume is recoverable.
		d.recoverSnapshot(context.Background(), r.ID, r.Snapshot)

		sb := d.sandboxFromRecord(r, now)
		sb.State = StatePaused
		sb.Snapshot = rt.SnapshotRef{Path: r.Snapshot}
		// Carry the idle clock across the restart: for a persistent sandbox
		// LastActiveAt is the abandonment clock, and resetting it on every
		// restart would mean a node that reboots weekly never abandons anything.
		sb.LastActiveAt = lastActive(r.UpdatedAt, now)
		sb.meter.freeze(now)

		d.mu.Lock()
		d.boxes[r.ID] = sb
		rec := d.recordOf(sb)
		d.mu.Unlock()
		d.persistRec(ctx, rec) // state may have changed from running to paused
		d.log().Info("sandbox restored as paused", "sandbox_id", r.ID, "previous_state", r.State)
	}

	// Sweep instances no record claims.
	if sw, ok := d.cfg.Runtime.(rt.Sweeper); ok {
		d.mu.RLock()
		known := make([]string, 0, len(d.boxes))
		for id := range d.boxes {
			known = append(known, id)
		}
		d.mu.RUnlock()
		sw.SweepUnknown(known)
	}
}

// sandboxFromRecord rebuilds what a persisted record describes, minus the three
// things that say how this restart found it: State, the live instance and the
// idle clock. Both restart paths -- adopted running, restored paused -- carry
// the same twenty fields out of the row, and a field added to one and forgotten
// in the other is a sandbox that comes back missing its cap or its owner.
func (d *Daemon) sandboxFromRecord(r PersistedSandbox, now time.Time) *Sandbox {
	vcpu := r.VCPU
	if vcpu <= 0 {
		vcpu = d.cfg.DefaultVCPU
	}
	memMiB := r.MemMiB
	if memMiB <= 0 {
		memMiB = d.cfg.DefaultMem
	}
	return &Sandbox{
		ID: r.ID, Template: r.Template, Name: r.Name, Slug: r.Slug, OrgID: r.OrgID,
		CreatedAt: time.Unix(r.CreatedAt, 0), ExpiresAt: timeOrZero(r.ExpiresAt),
		PublicPort: r.PublicPort, StartCmd: r.StartCmd, Env: r.Env,
		VCPU: vcpu, MemMiB: memMiB, StorageMB: r.StorageMB,
		IdleTimeout:      time.Duration(r.IdleTimeoutSecs) * time.Second,
		HibernateTimeout: time.Duration(r.HibernateTimeoutSecs) * time.Second,
		BackupRetention:  time.Duration(r.BackupRetentionSecs) * time.Second,
		IdentityHash:     r.IdentityHash,
		Networks:         r.Networks,
		CreatedBy:        r.CreatedBy,
		Visibility:       r.Visibility,
		pauseArchive:     r.PauseArchive,
		// The meter restarts at zero; durable totals live in the store.
		meter: newMeter(vcpu, memMiB, r.StorageMB, now),
	}
}

// adoptRunning reattaches a live instance to its persisted record as running.
func (d *Daemon) adoptRunning(ctx context.Context, r PersistedSandbox, inst rt.Instance, now time.Time) {
	sb := d.sandboxFromRecord(r, now)
	sb.State = StateRunning
	sb.inst = inst
	sb.LastActiveAt = now

	d.mu.Lock()
	d.boxes[r.ID] = sb
	rec := d.recordOf(sb)
	d.mu.Unlock()
	// The daemon restarted; the runtime membership table is empty even though
	// the sandbox is still up, so an adopted sandbox has to rejoin.
	if len(r.Networks) > 0 {
		if err := d.pushNetworks(ctx, r.ID); err != nil {
			d.log().Warn("adopt: could not restore network membership", "sandbox_id", r.ID, "error", err)
		}
	}
	d.persistRec(ctx, rec)
	d.log().Info("sandbox restored as running (adopted live instance)", "sandbox_id", r.ID)
}

// Shutdown pauses every running sandbox so the next Start can restore it, then
// stops the reconciler. Instances that cannot be paused are stopped anyway.
func (d *Daemon) Shutdown(ctx context.Context) {
	d.mu.RLock()
	var ids []string
	for id, sb := range d.boxes {
		// Standby too: Pause thaws first, so a frozen sandbox is written to disk
		// rather than left holding RAM for a process that is about to exit.
		if sb.State == StateRunning || sb.State == StateStandby || sb.State == StateResuming {
			ids = append(ids, id)
		}
	}
	d.mu.RUnlock()

	canSnapshot := d.cfg.Runtime != nil && d.cfg.Runtime.Capabilities().Snapshot
	// A runtime whose instances outlive the daemon and are adopted by the next
	// one loses nothing by leaving a sandbox running; stopping it after a
	// failed pause left no snapshot and no instance, and the restart dropped it.
	_, adoptable := d.cfg.Runtime.(rt.Adopter)
	pauseCtx := context.WithValue(ctx, shutdownKey{}, true)
	for _, id := range ids {
		if canSnapshot {
			if err := d.Pause(pauseCtx, id); err == nil {
				continue
			} else {
				d.log().Warn("shutdown: pause failed", "sandbox_id", id, "error", err)
			}
		}
		if adoptable {
			d.log().Warn("shutdown: leaving the sandbox running for the next daemon to adopt", "sandbox_id", id)
			continue
		}
		d.mu.Lock()
		var inst rt.Instance
		if sb, ok := d.boxes[id]; ok {
			inst = sb.inst
			sb.inst = nil
		}
		d.mu.Unlock()
		if inst != nil {
			if err := inst.Stop(ctx); err != nil {
				d.log().Warn("shutdown: stop failed", "sandbox_id", id, "error", err)
			}
		}
	}
	d.Stop()
}
