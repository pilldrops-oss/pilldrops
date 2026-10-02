package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// ErrNotRehomable refuses a rehome this node cannot honour: no archive, a
// sandbox that was not paused, or no snapshot of it in the archive.
var ErrNotRehomable = errors.New("sandbox cannot be rehomed here")

// Rehome takes over a paused sandbox whose node is gone: it pulls the capture
// its pause archived, boots it here under the same id, and so claims its
// record. The sandbox comes back running, as its resume asked.
//
// Only a paused sandbox: its archive is its latest state. One that was running
// when its node died has moved on since its last pause, and booting the older
// snapshot would hand its owner a machine silently rolled back.
func (d *Daemon) Rehome(ctx context.Context, id, org string) error {
	if d.cfg.SnapshotArchive == nil {
		return fmt.Errorf("%w: this node archives no pause snapshots (SNAPSHOT_ARCHIVE)", ErrNotRehomable)
	}
	g, ok := d.cfg.Store.(recordGetter)
	if !ok {
		return fmt.Errorf("%w: this node has no shared store to read the record from", ErrNotRehomable)
	}
	// One rehome of a sandbox at a time: a concurrent one would replace, then
	// on its Create failing remove, the capture the first one boots from.
	for {
		d.mu.Lock()
		_, held := d.boxes[id]
		running, busy := d.rehoming[id]
		if !held && !busy {
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
		if held {
			return nil // already here: a retried rehome is a no-op
		}
		if !busy {
			break
		}
		select {
		case <-running:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r, found, err := g.SandboxRecord(ctx, id)
	if err != nil {
		return err
	}
	if !found || (org != "" && r.OrgID != org) {
		return rt.ErrNotFound
	}
	if r.State != string(StatePaused) {
		return fmt.Errorf("%w: it was %s, and only a paused sandbox's archive is its latest state",
			ErrNotRehomable, r.State)
	}
	// The archive must be the pause the record says it is: one an earlier
	// pause left behind, or one written before this record's latest pause,
	// would bring the sandbox back rolled back.
	a, ok := d.cfg.SnapshotArchive.(*BlobArchive)
	if !ok {
		return fmt.Errorf("%w: this node's archive is not one another node restores from", ErrNotRehomable)
	}
	if r.PauseArchive == "" {
		return fmt.Errorf("%w: no archived snapshot of its latest pause (its record names none)", ErrNotRehomable)
	}
	if got, found, err := a.PauseID(ctx, id); err != nil {
		return err
	} else if !found || got != r.PauseArchive {
		return fmt.Errorf("%w: no archived snapshot of its latest pause", ErrNotRehomable)
	}
	// The capture lands as a template of its own, whole, by one rename: a
	// directory holding rootfs.squashfs is bootable, so it must never be seen
	// half-written. Then the sandbox is created under its own id from it, the
	// path a move's target takes, which carries its config from its record.
	tmpl := rehomeTemplate(id)
	dir := d.snapshotDir(tmpl)
	stage := dir + ".staging"
	got, err := d.cfg.SnapshotArchive.Get(ctx, id, stage)
	if err != nil {
		return err
	}
	if !got {
		return fmt.Errorf("%w: no archived snapshot of it", ErrNotRehomable)
	}
	_ = os.RemoveAll(dir)
	if err := os.Rename(stage, dir); err != nil {
		_ = os.RemoveAll(stage)
		return fmt.Errorf("rehome %s: %w", id, err)
	}
	if _, err := d.Create(ctx, CreateSpec{
		ID: id, OrgID: r.OrgID, Template: tmpl, Snapshot: tmpl,
		VCPU: r.VCPU, MemMiB: r.MemMiB, StorageMB: r.StorageMB,
	}); err != nil {
		// Holding rootfs.squashfs, it would be listed to tenants as a template;
		// the archive keeps the capture for a retry.
		_ = os.RemoveAll(dir)
		return fmt.Errorf("rehome %s: boot from its archive: %w", id, err)
	}
	// It runs here now and its record says so, but when its memory is being
	// served from the archive (mem.lazy) the archive is that memory until the
	// prefetch completes: leave it for the next pause or the destroy to drop.
	if _, lazyErr := os.Stat(filepath.Join(dir, "mem.lazy")); lazyErr != nil {
		d.discardArchive(id)
	}
	d.log().Info("sandbox rehomed from its archive", "sandbox_id", id, "previous_node", r.NodeID)
	return nil
}

// rehomeTemplate is the template a rehome of sandbox id lands its capture as.
func rehomeTemplate(id string) string { return "snap-rehome-" + id }

// ownerChecker is the optional store lookup that tells a node a record it
// holds now belongs to another node.
type ownerChecker interface {
	OwnedElsewhere(ctx context.Context, id string) (bool, error)
}
