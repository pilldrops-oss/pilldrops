//go:build linux

package vm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// A rootfs is planted where the traversal lands, so "refused by the validator"
// is distinguishable from "refused because nothing was there".
func TestCreateRefusesATraversalTemplateWithoutStattingOutsideSnapshotDir(t *testing.T) {
	base := t.TempDir()
	snapDir := filepath.Join(base, "snapshots")
	if err := os.MkdirAll(filepath.Join(snapDir, "listed-built"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(snapDir, "listed-built", "rootfs.squashfs"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write rootfs: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(base, "outside"), 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	if err := os.WriteFile(filepath.Join(base, "outside", "rootfs.squashfs"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write bait: %v", err)
	}
	if got := filepath.Join(snapDir, "../outside", "rootfs.squashfs"); got != filepath.Join(base, "outside", "rootfs.squashfs") {
		t.Fatalf("the bait is not where the traversal lands (%s); this test would pass for the wrong reason", got)
	}

	m := &Manager{
		config: Config{
			SnapshotDir:   snapDir,
			OverlayDir:    filepath.Join(base, "overlays"),
			Templates:     []string{"listed-built"},
			MaxConcurrent: 4,
			MaxActive:     10,
		},
		active:        map[string]*Sandbox{},
		createLimiter: make(chan struct{}, 4),
		log:           zap.NewNop(),
	}

	for _, tmpl := range []string{"../outside", "../../etc", "sub/dir", ".."} {
		_, err := m.Create(context.Background(), CreateRequest{Template: tmpl})
		if !errors.Is(err, runtime.ErrInvalidTemplateName) {
			t.Errorf("Create(%q) = %v, want ErrInvalidTemplateName. A traversing id must be refused "+
				"BEFORE filepath.Join, which cleans it into a real path outside SnapshotDir.", tmpl, err)
		}
	}

	// Vacuity guard: a legal template must get past both checks and fail later for want of a hypervisor.
	_, err := m.Create(context.Background(), CreateRequest{Template: "listed-built"})
	if errors.Is(err, runtime.ErrInvalidTemplateName) {
		t.Fatalf("Create(listed-built) = %v — the validator rejects a legal id, so the loop above proves nothing", err)
	}
	if errors.Is(err, runtime.ErrTemplateUnknown) {
		t.Fatalf("Create(listed-built) = %v — the planted rootfs was not found, so this test is not exercising the join", err)
	}
	if err == nil {
		t.Fatal("Create(listed-built) succeeded without a hypervisor; this test can no longer tell the guards from the boot")
	}
}
