package daemon

import (
	"context"
	"errors"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// restoreFromStore reloads persisted sandboxes after a restart. A running,
// standby, lost or failed record whose instance still exists, live or stopped,
// is adopted, and a stopped one started again; otherwise a record with a
// snapshot comes back paused; stopped, expired or unrestorable records this node
// owns are dropped. A record is never dropped while its instance exists.
//
// It reports whether it finished. It stops at the first instance lookup the
// runtime cannot answer (dockerd still starting) and returns false without
// dropping, rolling back or sweeping anything: "could not list" is not "none".
// The reconciler calls it again; records restored on an earlier pass are skipped.
func (d *Daemon) restoreFromStore(ctx context.Context) bool {
	if d.cfg.Store == nil {
		return true
	}
	d.mu.Lock()
	if d.gone == nil {
		d.gone = map[string]bool{}
	}
	d.mu.Unlock()
	recs, err := d.cfg.Store.LoadSandboxes(ctx)
	if err != nil {
		d.log().Error("restore: load sandboxes failed; retrying", "error", err)
		return false
	}
	adopter, _ := d.cfg.Runtime.(rt.Adopter)
	now := d.clock.Now()
	type casualty struct {
		id string
		lv rt.Liveness
		rv rt.Reviver
	}
	var casualties []casualty
	// adopt looks the instance up; ok=false with a nil inst is "none".
	adopt := func(id string) (inst rt.Instance, ok bool) {
		inst, err := adopter.Adopt(context.Background(), id)
		if err != nil && !errors.Is(err, rt.ErrNotFound) {
			d.log().Error("restore: the runtime cannot list instances; leaving the rest of the records for the next pass",
				"sandbox_id", id, "error", err)
			return nil, false
		}
		return inst, true
	}
	complete := true
	for _, r := range recs {
		d.mu.RLock()
		_, restored := d.boxes[r.ID]
		d.mu.RUnlock()
		if restored {
			continue
		}
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
				inst, ok := adopt(r.ID)
				if !ok {
					complete = false
					break
				}
				if inst != nil {
					if err := inst.Stop(context.Background()); err != nil {
						d.log().Warn("restore: expired instance would not stop", "sandbox_id", r.ID, "error", err)
					}
				}
			}
			drop("past hard TTL")
			continue
		}
		if !complete {
			break
		}

		// A standby sandbox's instance is frozen, not stopped, so it outlives the
		// daemon exactly as a running one does and adopts the same way. It is
		// thawed rather than adopted frozen: nothing would ever unfreeze it,
		// since the state it came back in is "running".
		// "lost" is the control plane's mark on a node it reaped (store.StateLost).
		// This node is back, so a VM that survived is adopted, not swept.
		// "failed" is a row an older build kept for a container that would not
		// start again; it is adopted and retried, and a crash now destroys it.
		if (r.State == string(StateRunning) || r.State == string(StateStandby) ||
			r.State == "lost" || r.State == string(StateFailed)) && adopter != nil {
			inst, ok := adopt(r.ID)
			if !ok {
				complete = false
				break
			}
			if inst != nil {
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
				if !d.adoptRunning(ctx, r, inst, now) {
					continue
				}
				if lv, ok := inst.(rt.Liveness); ok && stopped {
					casualties = append(casualties, casualty{r.ID, lv, rv})
				}
				continue
			}
		}

		// Known gap: after a host reboot a firecracker sandbox lands here with no VM; never-paused is dropped, resumed pairs a stale snapshot with a newer overlay.
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
		if !d.claim(r.ID, sb) {
			d.mu.Unlock()
			continue
		}
		rec := d.recordOf(sb)
		d.mu.Unlock()
		d.persistRec(ctx, rec) // state may have changed from running to paused
		d.log().Info("sandbox restored as paused", "sandbox_id", r.ID, "previous_state", r.State)
	}

	// In the background: a start with retries and an app readiness wait per
	// container, on a node full of them, held Start -- and with it the HTTP
	// listener and heartbeat -- long enough for the control plane to reap the
	// node. The casualties are already in d.boxes, so the sweep below keeps them.
	if len(casualties) > 0 {
		d.mu.Lock()
		stopping := closed(d.reviveStop)
		if !stopping {
			d.revives.Add(1)
		}
		d.mu.Unlock()
		if !stopping {
			go func() {
				defer d.revives.Done()
				forEachTarget(context.Background(), len(casualties), lifecycleConcurrency, func(i int) {
					c := casualties[i]
					d.reviveCasualty(context.Background(), c.id, c.lv, c.rv)
				})
			}()
		}
	}

	if !complete {
		return false
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
	d.mu.Lock()
	d.gone = nil
	d.mu.Unlock()
	return true
}

// claim adds a restored sandbox unless it is in d.boxes already or was removed
// while this restore was unfinished. The caller holds d.mu.
func (d *Daemon) claim(id string, sb *Sandbox) bool {
	if _, ok := d.boxes[id]; ok || d.gone[id] {
		return false
	}
	d.boxes[id] = sb
	return true
}

// forget removes a sandbox from d.boxes, remembering it while a restore is
// unfinished. The caller holds d.mu.
func (d *Daemon) forget(id string) {
	delete(d.boxes, id)
	if d.gone != nil {
		d.gone[id] = true
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
// false means the sandbox was restored already, or removed since its row was read.
func (d *Daemon) adoptRunning(ctx context.Context, r PersistedSandbox, inst rt.Instance, now time.Time) bool {
	sb := d.sandboxFromRecord(r, now)
	sb.State = StateRunning
	sb.inst = inst
	sb.LastActiveAt = now

	d.mu.Lock()
	if !d.claim(r.ID, sb) {
		d.mu.Unlock()
		return false
	}
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
	return true
}

// Shutdown pauses every running sandbox so the next Start can restore it, then
// stops the reconciler. Instances that cannot be paused are stopped anyway.
func (d *Daemon) Shutdown(ctx context.Context) {
	// A revive holds its sandbox's op lock for up to reviveTimeout; the pauses
	// below must not queue behind it.
	d.stopRevives()
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

// reviveStopWait bounds how long stopping waits for revives to give up; a var
// so tests can shrink it.
var reviveStopWait = 10 * time.Second

// stopRevives cancels restore's background revives and waits for them, at most
// reviveStopWait. A cancelled revive leaves its sandbox as it was, for the next
// start to adopt again.
func (d *Daemon) stopRevives() {
	d.reviveStopOnce.Do(func() {
		d.mu.Lock()
		close(d.reviveStop)
		d.mu.Unlock()
	})
	done := make(chan struct{})
	go func() {
		d.revives.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(reviveStopWait):
		d.log().Warn("stop: casualty revives still running; not waiting for them", "waited", reviveStopWait)
	}
}

// closed reports whether ch is closed.
func closed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
