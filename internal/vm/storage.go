//go:build linux

package vm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// minOverlayMB guards mkfs.ext4, which fails on an implausibly small device.
const minOverlayMB = 256

// overlaySizeMB resolves the overlay size for a declared cap: 0 or less means
// DefaultOverlayMB, below the floor is raised to minOverlayMB.
func overlaySizeMB(requested int) int {
	if requested <= 0 {
		return DefaultOverlayMB
	}
	if requested < minOverlayMB {
		return minOverlayMB
	}
	return requested
}

// createOverlay creates the sandbox's sparse ext4 overlay, sized to sizeMB, and
// chowns it to uid so the non-root VMM can open it O_RDWR. The cap is the
// filesystem size, which the guest cannot grow from inside.
func createOverlay(sandboxID string, uid, sizeMB int, cfg Config) (string, error) {
	path := filepath.Join(cfg.OverlayDir, sandboxID+".ext4")
	if err := makeExt4(path, uid, overlaySizeMB(sizeMB)); err != nil {
		return "", fmt.Errorf("overlay: %w", err)
	}
	return path, nil
}

// makeExt4 creates a sparse ext4 image of sizeMB at path, owned by uid and
// readable by nobody else. On error nothing is left behind.
func makeExt4(path string, uid, sizeMB int) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create image file: %w", err)
	}
	_ = f.Close()

	if err := os.Truncate(path, int64(sizeMB)<<20); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("truncate: %w", err)
	}

	if out, err := exec.CommandContext(context.Background(), "mkfs.ext4", "-F", "-q", path).CombinedOutput(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("mkfs.ext4: %s: %w", strings.TrimSpace(string(out)), err)
	}

	if err := os.Chown(path, uid, uid); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("chown image to %d: %w", uid, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("chmod image: %w", err)
	}
	return nil
}

// ownVolumes hands each attached volume to the VMM's uid. A volume belongs to
// one sandbox at a time, so the chown takes it from the previous holder.
func ownVolumes(paths []string, uid int) error {
	for _, p := range paths {
		if err := os.Chown(p, uid, uid); err != nil {
			return fmt.Errorf("volume %s: %w", filepath.Base(p), err)
		}
	}
	return nil
}

// copySparse copies src to dst preserving holes. An overlay is almost entirely
// holes, so io.Copy would write gigabytes of zeros per restore; the stdlib has
// no hole-aware copy.
func copySparse(src, dst string) error {
	out, err := exec.CommandContext(context.Background(), "cp", "--sparse=always", "--reflink=auto", src, dst).CombinedOutput()
	if err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("cp %s %s: %s: %w", src, dst, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// deleteOverlay removes a sandbox's writable image and, for a restored sandbox,
// its per-sandbox directory. os.Remove fails harmlessly if the directory is not empty.
func deleteOverlay(path string) {
	if path == "" {
		return
	}
	_ = os.Remove(path)
	dir := filepath.Dir(path)
	if filepath.Base(filepath.Dir(dir)) == restoreOverlaySubdir {
		_ = os.RemoveAll(dir) // also holds the sandbox's memory lineage
	}
}

// DiskUsagePercent returns how full the overlay directory partition is.
func DiskUsagePercent(overlayDir string) float64 {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(overlayDir, &stat); err != nil {
		return 0
	}
	total := float64(stat.Blocks) * float64(stat.Bsize)
	free := float64(stat.Bfree) * float64(stat.Bsize)
	return (1 - free/total) * 100
}
