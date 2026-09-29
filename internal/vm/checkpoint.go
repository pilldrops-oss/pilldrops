//go:build linux

package vm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"go.uber.org/zap"
)

// ckptOverlayName is the disk image inside a checkpoint directory. Fixed, so a
// restore need not consult meta.json to find it.
const ckptOverlayName = "overlay.ext4"

// captureLive writes a running sandbox's memory and state into dir and copies
// its disk to overlayDst, with the guest stopped only for those two writes; it
// is running again when this returns. Fork and checkpoint share it — there is
// deliberately no second capture path. An empty overlayDst skips the disk copy.
func (m *Manager) captureLive(ctx context.Context, sb *Sandbox, dir, overlayDst string) (paused time.Duration, kind string, copyMs int64, err error) {
	t0 := time.Now()
	if err := sb.Machine.PauseVM(ctx); err != nil {
		return 0, "", 0, fmt.Errorf("pause %s: %w", sb.ID, err)
	}
	snap, snapErr := m.beginMemorySnapshot(ctx, sb, dir)
	if snapErr == nil && overlayDst != "" {
		t1 := time.Now()
		if cerr := copySparse(sb.OverlayPath, overlayDst); cerr != nil {
			snapErr = fmt.Errorf("copy overlay: %w", cerr)
		}
		copyMs = time.Since(t1).Milliseconds()
	}
	resumeErr := sb.Machine.ResumeVM(ctx)
	paused = time.Since(t0)
	if resumeErr != nil {
		// The captured sandbox is the one that matters; a stranded one is logged loudly.
		m.log.Error("capture_resume_failed", zap.String("sandbox_id", sb.ID), zap.Error(resumeErr))
	}
	if snapErr != nil {
		return paused, "", copyMs, snapErr
	}
	if resumeErr != nil {
		return paused, "", copyMs, fmt.Errorf("resume %s after capture: %w", sb.ID, resumeErr)
	}
	// finish merges the diff onto the base; safe with the guest already running.
	if err := snap.finish(); err != nil {
		return paused, "", copyMs, fmt.Errorf("memory image: %w", err)
	}
	return paused, snap.kind, copyMs, nil
}

// CheckpointSandbox writes a named save point of a running sandbox into dir:
// state, mem, overlay.ext4 and meta.json, meta last so a half-written directory
// never reads as ready. The sandbox keeps running. dir is the caller's to name,
// create and replace atomically.
func (m *Manager) CheckpointSandbox(ctx context.Context, sandboxID, dir string) (paused time.Duration, size int64, err error) {
	m.mu.RLock()
	sb, ok := m.active[sandboxID]
	pressure := m.diskPressure
	m.mu.RUnlock()
	if !ok {
		return 0, 0, ErrSandboxNotFound
	}
	if pressure {
		return 0, 0, ErrDiskPressure
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, 0, fmt.Errorf("mkdir checkpoint dir: %w", err)
	}
	paused, kind, copyMs, err := m.captureLive(ctx, sb, dir, filepath.Join(dir, ckptOverlayName))
	if err != nil {
		return paused, 0, err
	}
	size = dirSize(dir)
	// OverlayPath is the LIVE disk: a restore copies the checkpoint's image back
	// over it, so the sandbox keeps the path its memory image was taken against.
	meta := m.snapMetaOf(sandboxID, sb)
	meta.Kind, meta.SizeBytes, meta.CreatedAt = kind, size, time.Now().UTC()
	if err := writeSnapMeta(dir, meta); err != nil {
		return paused, size, fmt.Errorf("write checkpoint meta: %w", err)
	}
	m.log.Info("sandbox_checkpointed",
		zap.String("sandbox_id", sandboxID),
		zap.String("checkpoint_dir", dir),
		zap.Int64("paused_ms", paused.Milliseconds()),
		zap.Int64("overlay_copy_ms", copyMs),
		zap.String("snapshot_kind", kind),
		zap.Int64("size_bytes", size),
	)
	return paused, size, nil
}

// ParkSandbox stops a sandbox's VMM and namespace but keeps its disk, so a
// checkpoint restore can boot a fresh VMM over the same overlay path. It is
// SnapshotSandbox without the snapshot: the caller already holds a memory image
// and would only be overwriting it.
//
// Unlike SnapshotSandbox the entry stays in m.active, replaced by the restore
// that follows. A restore that fails then still leaves a Destroy that reclaims
// the overlay, lineage, namespace and records; dropping the entry here would
// strand all of them with nothing keyed to find them by.
func (m *Manager) ParkSandbox(ctx context.Context, sandboxID string) error {
	m.mu.RLock()
	sb, ok := m.active[sandboxID]
	m.mu.RUnlock()
	if !ok {
		return ErrSandboxNotFound
	}
	sb.stopTimer()
	if sb.Envd != nil {
		sb.Envd.Close()
	}
	// A park whose VMM will not stop is a park the caller would overwrite this
	// sandbox's overlay under, and then boot a second VMM over. RestoreCheckpoint
	// does exactly that on the next two lines, so refuse rather than warn: a
	// failed rollback the caller can retry beats a corrupted disk it cannot.
	//
	// Only a VMM still running is fatal. Stop reports an error for a process that
	// has already gone, which is the common case and harmless; Alive is what
	// separates them. It answers true for an unrecorded pid, so an unknown state
	// refuses too — the wrong answer there costs a retry, the other costs the disk.
	if err := sb.Machine.Stop(ctx); err != nil {
		if m.Alive(sandboxID) {
			m.log.Error("park_stop_failed_vmm_alive",
				zap.String("sandbox_id", sandboxID), zap.Error(err))
			return fmt.Errorf("park %s: its VMM is still running after stop: %w", sandboxID, err)
		}
		m.log.Warn("park_stop_failed", zap.String("sandbox_id", sandboxID), zap.Error(err))
	}
	if sb.NetNS != "" {
		teardownNetNS(sb.NetNS)
	}
	m.log.Info("sandbox_parked", zap.String("sandbox_id", sandboxID), zap.String("overlay", sb.OverlayPath))
	return nil
}

// FreezeVM stops a sandbox's vCPUs, keeping its memory, processes and open
// sockets resident on this node. Standby: the sandbox stays in m.active and
// keeps its slot, its namespace and its address. ThawVM starts it again.
func (m *Manager) FreezeVM(ctx context.Context, sandboxID string) error {
	m.mu.RLock()
	sb, ok := m.active[sandboxID]
	m.mu.RUnlock()
	if !ok {
		return ErrSandboxNotFound
	}
	return sb.Machine.PauseVM(ctx)
}

// ThawVM starts a frozen sandbox's vCPUs again.
func (m *Manager) ThawVM(ctx context.Context, sandboxID string) error {
	m.mu.RLock()
	sb, ok := m.active[sandboxID]
	m.mu.RUnlock()
	if !ok {
		return ErrSandboxNotFound
	}
	return sb.Machine.ResumeVM(ctx)
}

// RestoreCheckpoint rolls sandboxID back to the checkpoint in ckptDir in place:
// the VMM stops keeping the disk, the checkpoint's disk image is copied back
// over the live overlay, and the sandbox is restored from the checkpoint's
// memory image with the same id, address and token.
//
// The checkpoint is read, never consumed: RestoreSandbox hard-links state and
// mem into the jailer chroot rather than moving them, so the same checkpoint
// restores any number of times.
func (m *Manager) RestoreCheckpoint(ctx context.Context, sandboxID, ckptDir string, vcpu, memMB int) (*Sandbox, error) {
	meta, err := readSnapMeta(ckptDir)
	if err != nil {
		return nil, fmt.Errorf("read checkpoint meta: %w", err)
	}
	img := filepath.Join(ckptDir, ckptOverlayName)
	if _, err := os.Stat(img); err != nil {
		return nil, fmt.Errorf("checkpoint disk image missing: %w", err)
	}
	if err := m.ParkSandbox(ctx, sandboxID); err != nil {
		return nil, err
	}
	// cp truncates the destination, so no block of the newer disk survives under
	// a hole in the checkpoint's image.
	if err := copySparse(img, meta.OverlayPath); err != nil {
		return nil, fmt.Errorf("roll disk back: %w", err)
	}
	return m.RestoreSandbox(ctx, sandboxID, ckptDir, vcpu, memMB)
}

// dirSize is the disk a checkpoint directory actually occupies: allocated
// blocks, not apparent size. Both the memory image and the overlay are mostly
// holes, so the apparent figure (1.5 GiB for a 1.5 GiB guest, every time) says
// nothing about what the cap of ten is protecting. `du -B1` agrees with this.
//
// ponytail: one level deep, which is all a checkpoint has.
func dirSize(dir string) int64 {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var total int64
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			total += st.Blocks * 512
			continue
		}
		total += info.Size()
	}
	return total
}
