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

// The image check must apply to listed templates too; a listed-but-unbuilt one
// otherwise fails inside the jailer as a retryable 503 that leaks a host path.
func TestUnbuiltTemplateIsBadRequestNotUnavailable(t *testing.T) {
	dir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(dir, "listed-built"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "listed-built", "rootfs.squashfs"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write rootfs: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "listed-unbuilt"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	m := &Manager{
		config: Config{
			SnapshotDir:   dir,
			OverlayDir:    filepath.Join(dir, "overlays"),
			Templates:     []string{"listed-built", "listed-unbuilt"},
			MaxConcurrent: 4,
			MaxActive:     10,
		},
		active:        map[string]*Sandbox{},
		createLimiter: make(chan struct{}, 4),
		log:           zap.NewNop(),
	}

	for _, tmpl := range []string{"listed-unbuilt", "unlisted-unbuilt"} {
		_, err := m.Create(context.Background(), CreateRequest{Template: tmpl})
		if !errors.Is(err, runtime.ErrTemplateUnknown) {
			t.Errorf("Create(%q) on a template with no image returned %v, want ErrTemplateUnknown. "+
				"Anything else reaches the boot path and is answered as a retryable 503 with the "+
				"host's image path in the body.", tmpl, err)
		}
	}

	// Vacuity guard: a built template must get past this check and fail later for want of a hypervisor.
	_, err := m.Create(context.Background(), CreateRequest{Template: "listed-built"})
	if err == nil {
		t.Fatal("Create on a built template unexpectedly succeeded without a hypervisor; " +
			"this test can no longer tell the image check from the boot")
	}
	if errors.Is(err, runtime.ErrTemplateUnknown) {
		t.Fatalf("Create on a BUILT template returned ErrTemplateUnknown (%v) — the image check "+
			"is rejecting everything, so the test above proves nothing", err)
	}
}
