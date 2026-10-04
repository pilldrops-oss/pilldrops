//go:build linux

package vm

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// ErrRootfsMismatch: the capture was taken against a different build of its
// template's read-only root image than this node holds under that name.
//
// A capture's overlay is a delta against one exact root image, and its memory
// image holds page-cache state that refers into that same image. Restore it
// over a different build and the VM comes up, reports boot_type=snapshot and
// hands back an intact overlay -- while every binary the guest has to read from
// the root image returns EIO. Silent corruption presented as success, measured
// on a two-node fleet.
//
// The template NAME is not an identity: every node builds its own image from
// the Dockerfile and two runs of one Dockerfile are not byte-identical, so any
// two nodes disagree. The bytes are the identity, so the digest travels in the
// capture's meta.json and is checked before the VMM is started.
// The sentinel lives in internal/runtime so the API layer can render it without
// importing the VM engine.
var ErrRootfsMismatch = runtime.ErrRootfsMismatch

// captureRootfs is the root image a capture directory carries, "" when it
// carries none. A named snapshot fetched from the artifact store brings the
// very image it was taken against and lands it here; a local pause or
// checkpoint does not, and falls back to the template name.
func captureRootfs(srcDir string) string {
	p := filepath.Join(srcDir, "rootfs.squashfs")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// rootfsPath is where the VM layer resolves a template's root image -- the same
// join firecracker.go makes for the root drive, so the digest describes the very
// file the restore would boot.
func (m *Manager) rootfsPath(template string) string {
	return filepath.Join(m.config.SnapshotDir, template, "rootfs.squashfs")
}

// rootfsDigest hashes a template's root image.
func (m *Manager) rootfsDigest(template string) (string, error) {
	return digestOf(m.rootfsPath(template))
}

// rootfsSums memoises root-image digests by path+size+mtime; its own lock so
// hashing a 200 MB image never holds any other.
var (
	sumMu      sync.Mutex
	rootfsSums = map[string]string{}
)

// digestOf hashes one image. Memoised on (size, mtime) so a node pays the hash
// once per image rather than once per restore; an image is replaced by rename
// (templatebuild, and linkInto on the fetch path) so changed bytes are a
// changed mtime.
func digestOf(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	key := fmt.Sprintf("%s|%d|%d", path, fi.Size(), fi.ModTime().UnixNano())

	sumMu.Lock()
	sum, ok := rootfsSums[key]
	sumMu.Unlock()
	if ok {
		return sum, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }() //nolint:errcheck // read-only
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	sum = "sha256:" + hex.EncodeToString(h.Sum(nil))

	sumMu.Lock()
	rootfsSums[key] = sum
	sumMu.Unlock()
	return sum, nil
}

// checkRootfs refuses a restore about to boot a root image that is not the one
// the capture was taken against. rootfs is the image the restore will actually
// use -- the capture's own when it carries one, and "" when it falls back to
// whatever the template NAME resolves to on this node. It is that fallback the
// check exists for.
//
// A capture with no recorded digest is let through: it predates this record,
// and refusing every one of them would strand snapshots that restore correctly
// on the node that took them.
//
// An unreadable image is an error, never a pass. The whole point is that "the
// answer is empty" and "the check did not run" must not look the same.
func (m *Manager) checkRootfs(meta sandboxSnapMeta, srcDir, rootfs string) error {
	if meta.RootfsDigest == "" {
		return nil
	}
	path := rootfs
	if path == "" {
		path = m.rootfsPath(meta.Template)
	}
	have, err := digestOf(path)
	if err != nil {
		return fmt.Errorf("check the root image %s against capture %s: %w", path, srcDir, err)
	}
	if have == meta.RootfsDigest {
		return nil
	}
	return fmt.Errorf("%w: capture %s was taken against template %q root image %s, "+
		"but %s is %s. Restoring it here would return a running sandbox whose every "+
		"binary reads EIO. Capture and restore on the same node, or rebuild this "+
		"node's image from the one the capture was taken against",
		ErrRootfsMismatch, srcDir, meta.Template, meta.RootfsDigest, path, have)
}
