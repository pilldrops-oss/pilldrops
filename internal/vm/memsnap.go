//go:build linux

package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// memSnapshot is a memory+state capture in two halves: begin runs while the
// VM is paused and writes only what must be captured under the pause; finish
// runs afterwards and produces the full "mem" image the next restore needs.
type memSnapshot struct {
	m    *Manager
	sb   *Sandbox
	kind string // "full" | "diff"
	// base is the image this capture diffs against, taken off the sandbox
	// before the capture invalidates it.
	base   string
	state  string
	mem    string
	tmp    string
	diff   string
	WriteT time.Duration
}

// beginMemorySnapshot writes the paused VM's state and memory into dir.
//
// A full snapshot of a file-backed guest faults in every page (2.1 s for
// 512 MiB measured on the reference host, against 0.2 s to write the bytes).
// A VM with a base has dirty-page tracking on, so only the pages touched since
// are written here; finish merges them onto a reflink of the base, which on a
// CoW filesystem costs the dirty set, not the guest size.
func (m *Manager) beginMemorySnapshot(ctx context.Context, sb *Sandbox, dir string) (*memSnapshot, error) {
	chroot := sb.Machine.ChrootRoot()
	if chroot == "" {
		chroot = filepath.Join(jailerChrootDir(), sb.ID, "root")
	}
	s := &memSnapshot{
		m: m, sb: sb, kind: "diff",
		state: filepath.Join(dir, "state"),
		mem:   filepath.Join(dir, "mem"),
		// Written beside the target and renamed over it: the base may be the
		// previous "mem" in this very directory (a second pause of one sandbox).
		tmp:  filepath.Join(dir, "mem.tmp"),
		diff: filepath.Join(dir, "diff"),
	}
	t0 := time.Now()
	// Firecracker's dirty bitmap is reset by the capture below, so the old image
	// stops being something a diff can be merged onto the moment we ask for one.
	// Take it for finish() and clear it here: every path that gives up between
	// this line and finish() then costs the next capture a full image, rather
	// than merging a partial dirty set onto a stale base and calling it a memory
	// snapshot.
	s.base = sb.MemBase
	sb.MemBase = ""
	var err error
	if s.base == "" {
		s.kind = "full"
		err = sb.Machine.CreateSnapshot(ctx, "/state", "/mem")
	} else {
		err = sb.Machine.CreateDiffSnapshot(ctx, "/state", "/diff")
	}
	if err != nil {
		return nil, err
	}
	s.WriteT = time.Since(t0)
	if err := moveFile(filepath.Join(chroot, "state"), s.state); err != nil {
		return nil, fmt.Errorf("move state out of chroot: %w", err)
	}
	if s.kind == "full" {
		err = moveFile(filepath.Join(chroot, "mem"), s.tmp)
	} else {
		err = moveFile(filepath.Join(chroot, "diff"), s.diff)
	}
	if err != nil {
		return nil, fmt.Errorf("move memory out of chroot: %w", err)
	}
	return s, nil
}

// finish produces dir/mem and makes it the VM's dirty-tracking base. Safe to
// run with the VM already resumed: the captured files are immutable.
func (s *memSnapshot) finish() error {
	t0 := time.Now()
	var dirty int64
	if s.kind == "diff" {
		defer func() { _ = os.Remove(s.diff) }()
		if err := copySparse(s.base, s.tmp); err != nil {
			return fmt.Errorf("copy memory base: %w", err)
		}
		n, err := applyDiff(s.diff, s.tmp)
		if err != nil {
			_ = os.Remove(s.tmp)
			return fmt.Errorf("merge dirty pages onto base: %w", err)
		}
		dirty = n
	}
	if err := os.Rename(s.tmp, s.mem); err != nil {
		return err
	}
	// Firecracker's dirty bitmap resets at every snapshot, so the next diff is
	// relative to this image. Keep a link of our own: the directory may belong
	// to a child that is destroyed before we snapshot again.
	base, err := s.m.adoptMemBase(s.sb.ID, s.mem)
	if err != nil {
		return err
	}
	s.sb.MemBase = base
	s.m.log.Info("memory_snapshot",
		zap.String("sandbox_id", s.sb.ID),
		zap.String("kind", s.kind),
		zap.Int64("write_ms", s.WriteT.Milliseconds()),
		zap.Int64("merge_ms", time.Since(t0).Milliseconds()),
		zap.Int64("dirty_bytes", dirty),
	)
	return nil
}

// lineageDir holds a sandbox's own hard links to its memory bases.
func (m *Manager) lineageDir(sandboxID string) string {
	return filepath.Join(m.config.OverlayDir, restoreOverlaySubdir, sandboxID)
}

// memBaseLink is the one name a sandbox's memory base goes by. A fixed name
// rather than a stamped one: the replacement is a rename over it, so the base it
// replaces is dropped in the same step and no caller has to remember which link
// it owns.
const memBaseLink = "mem.base"

// adoptMemBase hard-links memPath into the sandbox's lineage directory and
// returns the path to track.
//
// A capture directory belongs to whoever made it and may be deleted at any time
// — a checkpoint its owner removes, a fork's source, a backup's staging — so a
// VM tracking dirty pages against a file inside one is a VM whose next pause,
// fork or snapshot fails for a reason that has nothing to do with it. The link
// costs no bytes and outlives the directory.
func (m *Manager) adoptMemBase(sandboxID, memPath string) (string, error) {
	dir := m.lineageDir(sandboxID)
	link := filepath.Join(dir, memBaseLink)
	if memPath == link {
		return link, nil // already ours
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp := link + ".new"
	_ = os.Remove(tmp)
	if err := os.Link(memPath, tmp); err != nil {
		// Different filesystem: keep pointing at the file itself; it outlives us
		// only as long as its owner does, so a later diff falls back to full.
		if errors.Is(err, unix.EXDEV) {
			return memPath, nil
		}
		return "", fmt.Errorf("link memory base: %w", err)
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("publish memory base: %w", err)
	}
	return link, nil
}

// applyDiff copies every data extent of the sparse diff file onto dst at the
// same offset, leaving dst's clean pages untouched. Returns the bytes copied.
func applyDiff(diff, dst string) (int64, error) {
	in, err := os.Open(diff)
	if err != nil {
		return 0, err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY, 0)
	if err != nil {
		return 0, err
	}
	defer func() { _ = out.Close() }()
	fd := int(in.Fd())
	var off, total int64
	for {
		start, err := unix.Seek(fd, off, unix.SEEK_DATA)
		if err != nil {
			if errors.Is(err, unix.ENXIO) {
				return total, nil // no more data past off
			}
			return total, fmt.Errorf("seek data: %w", err)
		}
		end, err := unix.Seek(fd, start, unix.SEEK_HOLE)
		if err != nil {
			return total, fmt.Errorf("seek hole: %w", err)
		}
		if _, err := out.Seek(start, io.SeekStart); err != nil {
			return total, err
		}
		if _, err := io.CopyN(out, io.NewSectionReader(in, start, end-start), end-start); err != nil {
			return total, fmt.Errorf("copy extent %d-%d: %w", start, end, err)
		}
		total += end - start
		off = end
	}
}
