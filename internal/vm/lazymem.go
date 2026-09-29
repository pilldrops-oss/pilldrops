//go:build linux

package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/pilldrops-oss/pilldrops/internal/runtime"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"

	"github.com/pilldrops-oss/pilldrops/internal/vm/uffd"
)

// Lazy memory: a snapshot restored with Firecracker's Uffd backend starts
// running before its memory image has been read; uffd.Server fills each page
// on first touch and prefetches the rest. FC_UFFD=1 turns it on.
// ponytail: an env switch while it is proven; a per-restore choice once the
// source can be the bucket.

const lazyBlockSize = 2 << 20

// lazyPendingMarker sits in a VM's chroot while its memory is still being
// served; adoption refuses a VM that still has it (adopt.go).
const lazyPendingMarker = "lazy.pending"

func lazyMemEnabled() bool { return os.Getenv("FC_UFFD") == "1" }

// uffdLoadHandler replaces the SDK's snapshot load (which only knows a memory
// file) with one naming the Uffd backend, served from src, or from the file at
// memPath when src is nil.
func uffdLoadHandler(memPath string, src runtime.MemSource) firecracker.Handler {
	return firecracker.Handler{
		Name: firecracker.LoadSnapshotHandlerName,
		Fn: func(ctx context.Context, m *firecracker.Machine) error {
			root := filepath.Join(m.Cfg.JailerCfg.ChrootBaseDir, filepath.Base(m.Cfg.JailerCfg.ExecFile),
				m.Cfg.JailerCfg.ID, "root")
			if m.Cfg.JailerCfg.UID == nil {
				return fmt.Errorf("lazy memory: the jailer has no uid to hand the socket to")
			}
			if m.Cfg.MachineCfg.MemSizeMib == nil || *m.Cfg.MachineCfg.MemSizeMib <= 0 {
				return fmt.Errorf("lazy memory: the VM has no memory size to bound its handshake by")
			}
			maxBytes := uint64(*m.Cfg.MachineCfg.MemSizeMib) << 20
			var f *os.File
			source := uffd.Source(src)
			if src == nil {
				var err error
				if f, err = os.Open(memPath); err != nil {
					return fmt.Errorf("lazy memory: open the memory image: %w", err)
				}
				source = uffd.FileSource{F: f}
			}
			closeFile := func() {
				if f != nil {
					_ = f.Close()
				}
			}
			pending := filepath.Join(root, lazyPendingMarker)
			if err := os.WriteFile(pending, nil, 0o600); err != nil {
				closeFile()
				return fmt.Errorf("lazy memory: mark pending: %w", err)
			}
			t0 := time.Now()
			ready, err := uffd.Listen(context.Background(), filepath.Join(root, "uffd.sock"),
				*m.Cfg.JailerCfg.UID, lazyBlockSize, maxBytes, source)
			if err != nil {
				closeFile()
				return fmt.Errorf("lazy memory: listen: %w", err)
			}
			body, _ := json.Marshal(map[string]any{ //nolint:errcheck // fixed shape
				"snapshot_path":         m.Cfg.Snapshot.SnapshotPath,
				"mem_backend":           map[string]string{"backend_type": "Uffd", "backend_path": "/uffd.sock"},
				"enable_diff_snapshots": m.Cfg.Snapshot.EnableDiffSnapshots,
				"resume_vm":             m.Cfg.Snapshot.ResumeVM,
			})
			// SocketPath is already the host path: the jailer rewrote it.
			if err := apiPut(ctx, m.Cfg.SocketPath, "/snapshot/load", body); err != nil {
				closeFile()
				return fmt.Errorf("failed to load a snapshot for VM (lazy memory): %w", err)
			}
			var srv *uffd.Server
			select {
			case srv = <-ready:
			case <-time.After(30 * time.Second):
				closeFile()
				return fmt.Errorf("lazy memory: firecracker loaded the snapshot but never handed over its memory")
			}
			if srv.Err() != nil {
				closeFile()
				return srv.Err()
			}
			resumed := time.Since(t0)
			go func() {
				err := srv.Wait()
				closeFile()
				if err == nil {
					_ = os.Remove(pending) // complete: the VM no longer needs this process
				}
				slog.Info("lazy memory complete", "component", "vm", "vm_id", m.Cfg.JailerCfg.ID,
					"running_after_ms", resumed.Milliseconds(), "complete_after_ms", time.Since(t0).Milliseconds(),
					"faults", srv.Faults, "prefetched", srv.Prefetched, "err", err)
			}()
			return nil
		},
	}
}

// apiPut is one Firecracker API call over its socket, for a body the SDK's
// models cannot express.
func apiPut(ctx context.Context, sock, path string, body []byte) error {
	c := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}, Timeout: 60 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://localhost"+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("PUT %s: %s: %s", path, resp.Status, bytes.TrimSpace(msg))
	}
	return nil
}
