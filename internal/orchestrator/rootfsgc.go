package orchestrator

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
	"github.com/pilldrops-oss/pilldrops/internal/artifact/header"
)

// Shared root images (rfs-<digest>/rootfs) are stored once and referenced by
// the rootfs header of every capture and pause archive taken against them, so
// no purge of one capture may delete one. This sweep deletes the ones nothing
// references any more.

const (
	// rootfsGCEvery is how often the leader sweeps.
	rootfsGCEvery = time.Hour
	// rootfsGCGrace spares an image younger than this: a publish stores the
	// image before it writes the header pointing at it, and a sweep between the
	// two must not delete what is about to be referenced.
	rootfsGCGrace = time.Hour
)

// isRootfsHeader reports the header of a root image: a published capture's
// (<build>/rootfs.header) or a pause archive's (pause/<id>/files/rootfs.squashfs.header).
func isRootfsHeader(key string) bool {
	return strings.HasSuffix(key, "/"+artifact.NameRootfs+artifact.HeaderSuffix) ||
		strings.HasSuffix(key, "/rootfs.squashfs.header")
}

// sweepSharedRootfs deletes every shared root image no header references and
// older than grace. It deletes nothing when any header cannot be read: a
// reference it failed to see is one it would break.
// ponytail: lists the whole bucket each sweep; index references in the store
// if buckets grow past what an hourly listing handles.
func sweepSharedRootfs(ctx context.Context, blobs artifact.StorageProvider, now time.Time, grace time.Duration) (int, error) {
	keys, err := blobs.List(ctx, "")
	if err != nil {
		return 0, err
	}
	referenced := map[string]bool{}
	var shared []string
	for _, k := range keys {
		switch {
		case strings.HasPrefix(k, "rfs-") && strings.HasSuffix(k, "/"+artifact.NameRootfs):
			shared = append(shared, k)
		case isRootfsHeader(k):
			rc, err := blobs.Get(ctx, k)
			if err != nil {
				return 0, fmt.Errorf("read %s: %w", k, err)
			}
			raw, err := io.ReadAll(io.LimitReader(rc, maxHeaderBytes))
			_ = rc.Close() //nolint:errcheck // read-only
			if err != nil {
				return 0, fmt.Errorf("read %s: %w", k, err)
			}
			hdr, err := header.Unmarshal(raw)
			if err != nil {
				return 0, fmt.Errorf("parse %s: %w", k, err)
			}
			for _, id := range hdr.BuildIDs {
				if strings.HasPrefix(id, "rfs-") {
					referenced[id] = true
				}
			}
		}
	}
	deleted := 0
	for _, k := range shared {
		if referenced[strings.TrimSuffix(k, "/"+artifact.NameRootfs)] {
			continue
		}
		info, err := blobs.Stat(ctx, k)
		if err != nil || now.Sub(info.Modified) < grace {
			continue
		}
		if err := blobs.Delete(ctx, k); err != nil {
			return deleted, fmt.Errorf("delete %s: %w", k, err)
		}
		deleted++
	}
	return deleted, nil
}

// StartRootfsGC sweeps shared root images hourly on the leader until ctx ends.
func (s *Server) StartRootfsGC(ctx context.Context) {
	go func() {
		t := time.NewTicker(rootfsGCEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			blobs := s.blobStore()
			if blobs == nil || !s.isReconcileLeader() {
				continue
			}
			n, err := sweepSharedRootfs(ctx, blobs, s.now(), rootfsGCGrace)
			if err != nil {
				slog.Warn("shared root image sweep stopped without deleting past the error",
					"component", "artifact", "deleted", n, "err", err)
				continue
			}
			if n > 0 {
				slog.Info("deleted shared root images nothing references", "component", "artifact", "count", n)
			}
		}
	}()
}
