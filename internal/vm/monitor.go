//go:build linux

package vm

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// RunMonitors starts background monitoring goroutines.
// Call this after Manager is initialized, before accepting requests.
func RunMonitors(ctx context.Context, mgr *Manager, log *zap.Logger) {
	go diskWatchdog(ctx, mgr, log)
	go orphanCleaner(ctx, mgr, log)
}

// diskWatchdog sets diskPressure while the overlay dir is above 80% full, so creates return 503.
func diskWatchdog(ctx context.Context, mgr *Manager, log *zap.Logger) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pct := DiskUsagePercent(mgr.config.OverlayDir)

			log.Info("disk_usage",
				zap.Float64("percent", pct),
				zap.Int("active_sandboxes", mgr.ActiveCount()),
			)

			if pct > 95 {
				log.Error("disk_critical_destroying_sandboxes",
					zap.Float64("percent", pct),
				)
				mgr.SetDiskPressure(true)
			} else if pct > 80 {
				log.Warn("disk_pressure_rejecting_creates",
					zap.Float64("percent", pct),
				)
				mgr.SetDiskPressure(true)
			} else {
				mgr.SetDiskPressure(false)
			}
		}
	}
}

// orphanCleaner reclaims leaked jailer chroots every 60 seconds.
//
// It deliberately does NOT sweep orphaned overlays. The sweep that used to
// live here was wrong in a way that destroys live data: it asked whether the
// overlay file's basename was in m.active, and the basename is the id the VM
// layer registered the sandbox under at boot — which stops matching m.active the
// moment a sandbox is paused (SnapshotSandbox removes the entry and keeps the
// overlay for resume) or resumed (RestoreSandbox re-registers under the
// daemon's id while the overlay keeps its boot-time name). Measured on the
// side-car the first time this goroutine was ever started: create, pause,
// wait 90 s, resume → "restore: snapshot overlay missing: stat
// /srv/nspx/ovl/<boot-id>.ext4: no such file or directory".
//
// A correct predicate has to ask whether any sandbox this daemon knows owns
// that overlay PATH, which needs the paused set back on Manager — the
// m.paused map add-sandbox-checkpoints §1 wrote and then deleted once it
// found this goroutine was never started. That is the prerequisite, and it is
// a pause/resume change, not a monitor change.
//
// sweepLeakedChroots is safe on its own terms: it asks /proc which jailer ids
// still back a live VMM and keeps a 3-minute grace, so it never races a boot.
func orphanCleaner(ctx context.Context, mgr *Manager, log *zap.Logger) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			mgr.sweepLeakedChroots()
		}
	}
}
