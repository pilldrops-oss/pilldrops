//go:build linux

package vm

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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

// A golden's memory holds its template image's block layout: rebake the
// template under the same name and restoring the old golden reads EIO. On the
// rig the first creates after a rebake all failed that way.
func TestARebakedTemplateRetiresItsGolden(t *testing.T) {
	cfg := Config{SnapshotDir: t.TempDir()}
	rootfs := filepath.Join(cfg.SnapshotDir, "base", "rootfs.squashfs")
	dir := snapshotDir("base", "v1", cfg)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootfs, []byte("bake one"), 0o644); err != nil {
		t.Fatal(err)
	}
	baked, err := digestOf(rootfs)
	if err != nil {
		t.Fatal(err)
	}
	golden := goldenIdentity{TapName: "fctap1", VMIP: "192.168.0.1", OverlayBase: "o.ext4",
		StorageMB: goldenStorageMB(cfg, "base"), RootfsDigest: baked}
	if err := writeGoldenIdentity(dir, golden); err != nil {
		t.Fatal(err)
	}
	_, memMB := defaultResources(cfg, "base")
	writeMem := func(mb int) {
		t.Helper()
		f, err := os.Create(filepath.Join(dir, "mem"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := f.Truncate(int64(mb) << 20); err != nil {
			t.Fatal(err)
		}
	}
	writeMem(memMB / 2)
	if goldenCurrent(dir, cfg, "base") {
		t.Fatal("a golden taken at an older memory default was served")
	}
	writeMem(memMB)
	if !goldenCurrent(dir, cfg, "base") {
		t.Fatal("a golden of the image the template has was not served")
	}

	if err := os.WriteFile(rootfs, []byte("bake two"), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Second)
	if err := os.Chtimes(rootfs, later, later); err != nil {
		t.Fatal(err)
	}
	if goldenCurrent(dir, cfg, "base") {
		t.Fatal("the golden of the old image was served against the rebaked one")
	}

	golden.RootfsDigest = ""
	if err := writeGoldenIdentity(dir, golden); err != nil {
		t.Fatal(err)
	}
	if goldenCurrent(dir, cfg, "base") {
		t.Fatal("a golden that records no image was served")
	}
}
