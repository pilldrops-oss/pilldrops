//go:build linux

package vm

import (
	"context"
	"fmt"
	"github.com/pilldrops-oss/pilldrops/internal/runtime"
	"net"
	"os"
	"path/filepath"
	"time"

	"go.uber.org/zap"

	"github.com/pilldrops-oss/pilldrops/internal/auth/sbtoken"
	"github.com/pilldrops-oss/pilldrops/internal/egress"
)

// ForkSource is what a fork restores from: a memory+state pair plus the disk
// image it was taken against, and the network identity baked into that memory.
type ForkSource struct {
	Template string
	// RootfsPath is the capture's own root image, when the capture carries one.
	// A fork of a live parent leaves it empty: the parent is booted off this
	// node's image for that template, which is by definition the right one.
	RootfsPath  string
	StatePath   string
	MemPath     string
	OverlayPath string
	TapName     string
	VMIP        string
	// Egress is the parent's policy, inherited: a fork of a confined sandbox
	// must not come up unconfined.
	Egress *egress.Rules
	// MemSource, when set, serves memory lazily and MemPath names no file.
	MemSource runtime.MemSource
}

// ForkSandbox copies a running sandbox into childID: disk, memory and every
// process. The parent pauses only while its memory is written and its overlay
// copied, then carries on. The child comes up in its own network namespace with
// its own envd token. Returns the child and how long the parent was paused.
func (m *Manager) ForkSandbox(ctx context.Context, parentID, childID string, vcpu, memMB int) (*Sandbox, time.Duration, error) {
	if !m.config.NetNS {
		return nil, 0, ErrForkNeedsNetNS
	}
	m.mu.RLock()
	parent, ok := m.active[parentID]
	diskPressure, activeCount := m.diskPressure, len(m.active)
	m.mu.RUnlock()
	if !ok {
		return nil, 0, ErrSandboxNotFound
	}
	if diskPressure {
		return nil, 0, ErrDiskPressure
	}
	if activeCount >= m.config.MaxActive {
		return nil, 0, ErrServerFull
	}

	dir := filepath.Join(m.config.OverlayDir, restoreOverlaySubdir, childID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, 0, fmt.Errorf("fork dir: %w", err)
	}
	src := ForkSource{
		Template:    parent.Template,
		StatePath:   filepath.Join(dir, "state"),
		MemPath:     filepath.Join(dir, "mem"),
		OverlayPath: filepath.Join(dir, filepath.Base(parent.OverlayPath)),
		TapName:     parent.TapName,
		VMIP:        parent.VMIP,
		Egress:      parent.Egress,
	}

	// The pause window holds only what must be captured with the guest still:
	// the memory write and the disk copy. Merging the diff onto its base runs
	// after the parent is back. Shared with CheckpointSandbox.
	paused, kind, copyMs, err := m.captureLive(ctx, parent, dir, src.OverlayPath)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, paused, err
	}

	child, err := m.restoreFork(childID, src, vcpu, memMB)
	if err != nil {
		return nil, paused, err
	}
	m.log.Info("sandbox_forked",
		zap.String("parent_id", parentID),
		zap.String("sandbox_id", childID),
		zap.Int64("parent_paused_ms", paused.Milliseconds()),
		zap.String("snapshot_kind", kind),
		zap.Int64("overlay_copy_ms", copyMs),
		zap.Int64("boot_ms", child.BootMs),
	)
	return child, paused, nil
}

// ForkFromSnapshot forks a paused sandbox: its snapshot directory (from
// SnapshotSandbox) and kept overlay are copied for the child, and the parent
// stays paused.
func (m *Manager) ForkFromSnapshot(ctx context.Context, srcDir, childID string, vcpu, memMB int) (*Sandbox, error) {
	return m.bootFromCapture(ctx, srcDir, childID, vcpu, memMB, "", nil)
}

// BootFromCapture boots a new sandbox out of a capture directory that came from
// somewhere else — a named snapshot fetched from the artifact store, rather than
// a parent paused on this node. Same mechanism as a fork of a paused parent; the
// difference is only what it is called and whose egress policy it takes.
func (m *Manager) BootFromCapture(ctx context.Context, srcDir, childID string, vcpu, memMB int, eg *egress.Rules) (*Sandbox, error) {
	return m.bootFromCapture(ctx, srcDir, childID, vcpu, memMB, "snapshot", eg)
}

// bootFromCapture is the shared body. bootType overrides what the child reports
// having done; egressOverride replaces the captured policy with the requesting
// create's, and nil keeps the captured one.
func (m *Manager) bootFromCapture(ctx context.Context, srcDir, childID string, vcpu, memMB int,
	bootType string, egressOverride *egress.Rules) (*Sandbox, error) {
	if !m.config.NetNS {
		return nil, ErrForkNeedsNetNS
	}
	// The same guard Create, ForkSandbox and CheckpointSandbox read, asked here
	// too: what follows is the largest write the node makes — a full copy of
	// the memory image, the machine state and the overlay disk — so a boot from
	// a capture on a full filesystem does not fail early with a retryable
	// refusal, it dies part-way through copySparse with a raw ENOSPC. Placed in
	// the shared body so ForkFromSnapshot and BootFromCapture are both covered.
	m.mu.RLock()
	pressure := m.diskPressure
	m.mu.RUnlock()
	if pressure {
		return nil, ErrDiskPressure
	}
	meta, err := readSnapMeta(srcDir)
	if err != nil {
		return nil, fmt.Errorf("read snapshot meta: %w", err)
	}
	// Before anything is copied: a capture restored against the wrong root image
	// boots into a guest that can read nothing. A fetched snapshot brings its
	// own image, so this normally compares the capture against itself; it is the
	// fallback path — an older build, or a local capture — that it catches.
	rootfs := captureRootfs(srcDir)
	if err := m.checkRestorable(meta, srcDir, rootfs); err != nil {
		return nil, err
	}
	dir := filepath.Join(m.config.OverlayDir, restoreOverlaySubdir, childID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("fork dir: %w", err)
	}
	// The disk the memory image was taken against. A capture directory carries
	// its own copy (CheckpointSandbox writes overlay.ext4 beside state and mem)
	// and meta.OverlayPath names the LIVE overlay on the node that took it —
	// which has moved on since, and on a node that only fetched the capture does
	// not exist at all. Prefer the directory's own copy whenever it is there.
	srcOverlay := meta.OverlayPath
	own := filepath.Join(srcDir, ckptOverlayName)
	if _, statErr := os.Stat(own); statErr == nil {
		srcOverlay = own
	}
	src := ForkSource{
		Template:   meta.Template,
		RootfsPath: rootfs,
		StatePath:  filepath.Join(dir, "state"),
		MemPath:    filepath.Join(dir, "mem"),
		// Named after the CAPTURED overlay, never after the file it is copied
		// from. Firecracker records the block device's backing file by name in
		// the snapshot state, so a child whose disk is called anything else
		// fails the load with "Error manipulating the backing file: No such
		// file or directory <captured name>". Measured on a cold node, where
		// the copy source is the fetched build's overlay.ext4.
		OverlayPath: filepath.Join(dir, filepath.Base(meta.OverlayPath)),
		TapName:     meta.TapName,
		VMIP:        meta.VMIP,
		// The pause snapshot carries the policy, so the copy is confined like the parent.
		Egress: meta.Egress,
	}
	if egressOverride != nil {
		src.Egress = egressOverride
	}
	copies := map[string]string{
		filepath.Join(srcDir, "state"): src.StatePath,
		filepath.Join(srcDir, "mem"):   src.MemPath,
		srcOverlay:                     src.OverlayPath,
	}
	// A capture whose memory stayed in the bucket boots with it served on
	// demand: nothing to copy, and the VM runs before the memory has arrived.
	if note, err := os.ReadFile(filepath.Join(srcDir, lazyMemNote)); err == nil {
		if _, statErr := os.Stat(filepath.Join(srcDir, "mem")); os.IsNotExist(statErr) {
			if runtime.LazyMemSource == nil {
				_ = os.RemoveAll(dir)
				return nil, fmt.Errorf("this capture's memory is in the bucket and this node cannot read it lazily")
			}
			ms, err := runtime.LazyMemSource(context.Background(), note)
			if err != nil {
				_ = os.RemoveAll(dir)
				return nil, fmt.Errorf("open the capture's memory in the bucket: %w", err)
			}
			src.MemSource = ms
			delete(copies, filepath.Join(srcDir, "mem"))
		}
	}
	// Copies, not links: the parent's files stay owned by the parent's uid and
	// must outlive this child. Reflink makes them free on a CoW filesystem.
	for from, to := range copies {
		if err := copySparse(from, to); err != nil {
			_ = os.RemoveAll(dir)
			return nil, fmt.Errorf("copy %s for fork: %w", filepath.Base(from), err)
		}
	}
	child, err := m.restoreFork(childID, src, vcpu, memMB)
	if err != nil {
		return nil, err
	}
	if bootType != "" {
		child.BootType = bootType
	}
	m.log.Info("sandbox_forked_from_snapshot",
		zap.String("snapshot_dir", srcDir),
		zap.String("sandbox_id", childID),
		zap.Int64("boot_ms", child.BootMs),
	)
	return child, nil
}

// restoreFork boots childID from src in a private namespace that reuses the
// baked tap name and guest address, pushes a fresh token, and registers it.
// src's files must be owned by the fork (they are chowned to the child's uid
// and removed once the VMM holds them).
func (m *Manager) restoreFork(childID string, src ForkSource, vcpu, memMB int) (*Sandbox, error) {
	dir := filepath.Dir(src.OverlayPath)
	fail := func(err error) (*Sandbox, error) {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	guestIP := net.ParseIP(src.VMIP)
	if guestIP == nil {
		return fail(fmt.Errorf("fork source has invalid guest address %q", src.VMIP))
	}
	uid := vmUID(m.config.NonRootVMM, childID)
	owned := []string{src.StatePath, src.OverlayPath}
	if src.MemSource == nil {
		owned = append(owned, src.MemPath)
	}
	for _, p := range owned {
		if err := os.Chown(p, uid, uid); err != nil {
			return fail(fmt.Errorf("chown %s to %d: %w", filepath.Base(p), uid, err))
		}
		if err := os.Chmod(p, 0o600); err != nil {
			return fail(fmt.Errorf("chmod %s: %w", filepath.Base(p), err))
		}
	}

	var token, tokenHash string
	if m.config.EnvdAuth {
		token, tokenHash = sbtoken.Mint()
	}

	t0 := time.Now()
	ns, err := setupNetNS(childID, uid, src.TapName, guestIP, src.Egress, m.log)
	if err != nil {
		return fail(fmt.Errorf("netns: %w", err))
	}
	p := restoreParams{
		SandboxID: childID, Template: src.Template, RootfsPath: src.RootfsPath,
		TapName: ns.TapName, VMIP: guestIP, NodeAddr: ns.NodeAddr,
		OverlayPath: src.OverlayPath, StatePath: src.StatePath, MemPath: src.MemPath,
		VCPU: vcpu, MemMB: memMB,
		NetNSPath: ns.NetNSPath, NetNSName: ns.Name,
		Token: token, BootType: "fork",
		MemSource: src.MemSource, LazyMem: src.MemSource != nil,
	}
	machine, err := m.bootSnapshot(p)
	if err != nil {
		teardownNetNS(ns.Name)
		return fail(fmt.Errorf("restore fork: %w", err))
	}
	failVM := func(err error) (*Sandbox, error) {
		// A VMM that will not stop outlives this directory; say so rather than
		// leaking it silently.
		if err := machine.Stop(context.Background()); err != nil {
			m.log.Warn("fork_cleanup_stop_failed", zap.String("sandbox_id", childID), zap.Error(err))
		}
		cleanJailerRoot(machine.ChrootRoot())
		teardownNetNS(ns.Name)
		return fail(err)
	}
	// The child's memory still holds the parent's credential; replace it before
	// anyone is told the child exists.
	if tokenHash != "" {
		if err := deliverCredential(machine.ChrootRoot(), childID, tokenHash, credPushDeadline); err != nil {
			return failVM(fmt.Errorf("fork %s: %w", childID, err))
		}
	}
	// Its own name, for the same reason it got its own address: the memory image
	// carries the captured machine's. Logged rather than fatal — a child with
	// the wrong hostname is visibly wrong and still usable, and tearing down a
	// healthy machine over it is the worse trade.
	if err := deliverGuestHostname(machine.ChrootRoot(), childID); err != nil {
		m.log.Warn("child_hostname_not_set", zap.String("sandbox_id", childID), zap.Error(err))
	}
	sb, err := m.attachRestored(p, machine, t0)
	if err != nil {
		return failVM(fmt.Errorf("envd not ready after fork: %w", err))
	}
	// The VMM holds the state through the chroot's hard link, so it goes now.
	// The memory file stays: it is the child's dirty-tracking base.
	_ = os.Remove(src.StatePath)
	return sb, nil
}

// lazyMemNote names where a capture's memory is when the file is not here.
const lazyMemNote = "mem.lazy"
