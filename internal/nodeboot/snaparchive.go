package nodeboot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
	"github.com/pilldrops-oss/pilldrops/internal/hoststats"
	"github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
)

// SnapshotArchive picks where pause snapshots are archived off the node, or
// nil to keep them node-local, in which case a paused sandbox dies with its
// node:
//
//	SNAPSHOT_ARCHIVE=bucket   the node's object store (SANDBOXD_ARTIFACT_STORE=s3),
//	                          which every node reads, so a pause resumes anywhere
//	SNAPSHOT_ARCHIVE_DIR=...  a directory, expected to be a shared network volume
func SnapshotArchive(blobs daemon.BlobStore, dataDir string) (daemon.SnapshotArchive, error) {
	if strings.EqualFold(Env("SNAPSHOT_ARCHIVE", ""), "bucket") {
		if blobs == nil {
			return nil, errors.New("SNAPSHOT_ARCHIVE=bucket needs a writable object store: " +
				"set SANDBOXD_ARTIFACT_STORE=s3 and SANDBOXD_ARTIFACT_S3_*")
		}
		lazy := Env("SANDBOXD_LAZY_MEMORY", "1") != "0"
		// A rehome may boot with its memory left in the bucket and served on
		// demand (vm/lazymem.go), which needs ranged reads of this store.
		if st, ok := blobs.(interface {
			artifact.Ranger
			Get(context.Context, string) (io.ReadCloser, error)
		}); ok && lazy {
			runtime.LazyMemSource = func(ctx context.Context, note []byte) (runtime.MemSource, error) {
				return daemon.OpenLazySource(ctx, st, note)
			}
		} else {
			lazy = false
		}
		slog.Info("snapshot archive", "component", "daemon", "store", blobs.Name(), "lazy_memory", lazy)
		return &daemon.BlobArchive{Store: blobs, CPUFeatures: hoststats.CPUFeatures(), Lazy: lazy,
			RootfsCache: filepath.Join(CacheDir(dataDir), "shared-rootfs")}, nil
	}
	if dir := Env("SNAPSHOT_ARCHIVE_DIR", ""); dir != "" {
		a, err := daemon.NewDirArchive(dir)
		if err != nil {
			return nil, fmt.Errorf("snapshot archive: %w", err)
		}
		slog.Info("snapshot archive", "component", "daemon", "dir", dir)
		return a, nil
	}
	return nil, nil
}
