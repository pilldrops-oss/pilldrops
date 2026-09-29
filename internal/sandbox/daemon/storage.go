package daemon

import (
	"context"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// storageCacheTTL bounds how often disk usage is re-measured; dashboards poll far faster than disk moves.
const storageCacheTTL = 60 * time.Second

// StorageUsedMB reports a sandbox's disk usage, cached for storageCacheTTL. ok
// is false when nothing has ever been measured. A paused sandbox returns its
// last measurement.
func (d *Daemon) StorageUsedMB(ctx context.Context, id string) (int, bool) {
	d.mu.RLock()
	sb, ok := d.boxes[id]
	if !ok {
		d.mu.RUnlock()
		return 0, false
	}
	inst := sb.inst
	mb, at := sb.diskMB, sb.diskAt
	d.mu.RUnlock()

	fresh := !at.IsZero() && time.Since(at) < storageCacheTTL
	if fresh || inst == nil {
		return mb, !at.IsZero()
	}
	du, can := inst.(rt.DiskUsager)
	if !can {
		return mb, !at.IsZero()
	}
	// Single-flight: when the cache expires every concurrent caller misses together.
	m, measured := d.diskSample.Do(ctx, id, time.Now(), func(ctx context.Context) (int, bool) {
		v, err := du.DiskUsageMB(ctx)
		return v, err == nil
	})
	if !measured {
		// Keep the stale value; a transient failure must not blank the meter.
		return mb, !at.IsZero()
	}
	d.mu.Lock()
	if sb2, still := d.boxes[id]; still {
		sb2.diskMB, sb2.diskAt = m, time.Now()
	}
	d.mu.Unlock()
	return m, true
}
