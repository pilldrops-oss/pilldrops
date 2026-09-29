package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// SnapshotArchive is durable off-node storage for pause snapshots; nil keeps them node-local.
// A snapshot may be a single file or a directory, so implementations must handle both.
type SnapshotArchive interface {
	// Put copies the snapshot at src into the archive under id. A concurrent Get
	// must never see a half-written snapshot.
	Put(ctx context.Context, id, src string) error
	// Get restores the archived snapshot for id to dst; ok is false when none exists.
	Get(ctx context.Context, id, dst string) (bool, error)
	// Delete removes the archived snapshot.
	Delete(ctx context.Context, id string) error
}

// DirArchive stores snapshots under a directory, expected to be a network volume in production.
type DirArchive struct{ Root string }

// NewDirArchive creates the archive root.
func NewDirArchive(root string) (*DirArchive, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("archive root: %w", err)
	}
	return &DirArchive{Root: root}, nil
}

func (a *DirArchive) path(id string) string { return filepath.Join(a.Root, id) }

func (a *DirArchive) Put(_ context.Context, id, src string) error {
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("archive put %s: %w", id, err)
	}
	// Stage then rename so a reader never sees a partial copy.
	tmp := a.path(id) + ".tmp"
	_ = os.RemoveAll(tmp)
	if err := copyPath(src, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("archive put %s: %w", id, err)
	}
	final := a.path(id)
	// Rename onto an existing directory fails, so clear the target first.
	_ = os.RemoveAll(final)
	if err := os.Rename(tmp, final); err != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("archive put %s: %w", id, err)
	}
	return nil
}

func (a *DirArchive) Get(_ context.Context, id, dst string) (bool, error) {
	src := a.path(id)
	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, err
	}
	_ = os.RemoveAll(dst)
	if err := copyPath(src, dst); err != nil {
		_ = os.RemoveAll(dst) // never leave a half-written snapshot to restore
		return false, fmt.Errorf("archive get %s: %w", id, err)
	}
	return true, nil
}

func (a *DirArchive) Delete(_ context.Context, id string) error {
	return os.RemoveAll(a.path(id))
}

// archiveSnapshot copies a pause snapshot to the archive. Failure is logged, not
// fatal: the sandbox is still locally resumable. Runs synchronously so a node
// loss right after pause cannot lose the copy.
func (d *Daemon) archiveSnapshot(ctx context.Context, id, path string) {
	if d.cfg.SnapshotArchive == nil || path == "" {
		return
	}
	if err := d.cfg.SnapshotArchive.Put(ctx, id, path); err != nil {
		d.log().Warn("archive snapshot failed", "sandbox_id", id, "error", err)
		return
	}
	d.log().Info("archived snapshot", "sandbox_id", id)
}

// recoverSnapshot pulls an archived snapshot back to dst when the local copy is
// missing. Reports whether a snapshot is present at dst afterwards.
func (d *Daemon) recoverSnapshot(ctx context.Context, id, dst string) bool {
	if _, err := os.Stat(dst); err == nil {
		return true
	}
	// A portable archive holds a move capture, not this path's pause snapshot;
	// it comes back through Rehome, as a boot, never through this restore.
	if d.cfg.SnapshotArchive == nil || d.portableArchive() {
		return false
	}
	ok, err := d.cfg.SnapshotArchive.Get(ctx, id, dst)
	if err != nil {
		d.log().Warn("recover snapshot failed", "sandbox_id", id, "error", err)
		return false
	}
	if ok {
		d.log().Info("recovered snapshot from archive", "sandbox_id", id)
	}
	return ok
}

// copyPath copies a file or a whole directory tree from src to dst.
func copyPath(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return copyFile(src, dst, info.Mode())
	}
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if fi.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !fi.Mode().IsRegular() {
			return nil // skip sockets and devices
		}
		return copyFile(p, target, fi.Mode())
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	// Check the Close error: it carries the flush failure for the tail of the file.
	if err := out.Close(); err != nil {
		return err
	}
	return nil
}

// pauseCaptureName is the internal capture a pause takes for a portable archive.
const pauseCaptureName = ExportDirPrefix + "pause"

// portableArchive reports an archive another node restores from (BlobArchive),
// which gets the full capture rather than this node's pause snapshot.
func (d *Daemon) portableArchive() bool {
	p, ok := d.cfg.SnapshotArchive.(interface{ Portable() bool })
	return ok && p.Portable()
}

// addCaptureRootfs puts the root image a capture was taken against into it as
// rootfs.squashfs. A capture directory leaves it out (it is large and every
// capture on the node shares it), so a node restoring it would fall back to its
// own build of the template, which is different bytes, and the restore refuses.
// Hard-linked: the templates and captures share the node's data disk.
func addCaptureRootfs(capDir string) error {
	raw, err := os.ReadFile(filepath.Join(capDir, "meta.json"))
	if err != nil {
		return err
	}
	var ident struct {
		Template string `json:"template"`
	}
	if err := json.Unmarshal(raw, &ident); err != nil {
		return err
	}
	if err := rt.ValidateTemplateName(ident.Template); err != nil {
		return err
	}
	src := filepath.Join(templates.Dir(), ident.Template, "rootfs.squashfs")
	dst := filepath.Join(capDir, "rootfs.squashfs")
	_ = os.Remove(dst)
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	return copyPath(src, dst)
}

// archivePause archives a pause's portable capture under a fresh id and
// returns it. The record carries the id while the sandbox stays paused, and a
// rehome takes the archive only when the two match, which is what keeps an
// older archive from ever coming back as the sandbox's state.
func (d *Daemon) archivePause(ctx context.Context, id, capDir string) (string, bool) {
	a, ok := d.cfg.SnapshotArchive.(*BlobArchive)
	if !ok {
		return "", false
	}
	var b [12]byte
	_, _ = rand.Read(b[:])
	pid := hex.EncodeToString(b[:])
	if err := a.PutPause(ctx, id, capDir, pid); err != nil {
		d.log().Warn("archive snapshot failed; this pause is not rehomable", "sandbox_id", id, "error", err)
		return "", false
	}
	d.log().Info("archived snapshot", "sandbox_id", id, "pause_archive", pid)
	return pid, true
}

// discardArchive deletes an archive whose sandbox has moved on. Tidying, not
// safety (the record's id already refuses it), so it never holds up the
// caller and a bucket that is down only leaves the bytes for later.
func (d *Daemon) discardArchive(id string) {
	if !d.portableArchive() {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := d.cfg.SnapshotArchive.Delete(ctx, id); err != nil {
			d.log().Info("an archived pause the sandbox moved on from stays in the bucket",
				"sandbox_id", id, "error", err)
		}
	}()
}

type shutdownKey struct{}

// shuttingDown reports a pause made by Shutdown, which has a deadline to keep
// and skips the portable capture and its upload.
func shuttingDown(ctx context.Context) bool { return ctx.Value(shutdownKey{}) != nil }
