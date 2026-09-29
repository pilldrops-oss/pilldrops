//go:build linux

package vm

import (
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// Without the storage clause a restore hands every sandbox a copy of the golden
// overlay regardless of the declared cap.
func TestRestoreServesOnlyTheGoldenStorageSize(t *testing.T) {
	const dv, dm, golden = 2, 1024, 1024
	cases := []struct {
		name      string
		storageMB int
		want      bool
	}{
		{"no declared size (defaults) is servable", 0, true},
		{"the golden's own size (the template default) is servable", golden, true},
		{"a smaller plan-capped 512MB must cold boot, not inherit a bigger overlay", 512, false},
		{"a larger-than-golden request must cold boot too", golden * 2, false},
	}
	for _, c := range cases {
		if got := restoreServes(dv, dv, dm, dm, c.storageMB, golden); got != c.want {
			t.Errorf("%s: restoreServes(storage=%d) = %v, want %v", c.name, c.storageMB, got, c.want)
		}
	}
	if restoreServes(4, dv, dm, dm, 0, golden) || restoreServes(dv, dv, 2048, dm, 0, golden) {
		t.Error("a custom vCPU/RAM request must cold boot; the golden is baked at the defaults")
	}
}

// The golden is built at the template's own default disk, so a default create
// (which asks for exactly that) restores instead of cold booting.
func TestGoldenIsBuiltAtTheTemplateDefaultStorage(t *testing.T) {
	cfg := Config{Resources: map[string]runtime.Resources{"base": {VCPUs: 1, MemoryMB: 512, StorageMB: 1024}}}
	if got := goldenStorageMB(cfg, "base"); got != 1024 {
		t.Fatalf("golden storage for base = %d, want its default 1024", got)
	}
	if got := goldenStorageMB(cfg, "unknown"); got != DefaultOverlayMB {
		t.Fatalf("a template with no declared storage = %d, want DefaultOverlayMB", got)
	}
	if !restoreServes(1, 1, 512, 512, 1024, goldenStorageMB(cfg, "base")) {
		t.Fatal("a default-size create of base is not served by its golden")
	}
}
