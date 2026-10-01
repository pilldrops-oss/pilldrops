package nodeboot

import (
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// python-3.11 is an alias with its own image; a create sizes it as its entry,
// so the VM's defaults for it must be the same or its golden never serves.
func TestTemplateResourcesCoverAliases(t *testing.T) {
	cat := templates.Catalog()
	res := templateResources(cat)
	ci, ok := templates.Resolve("python-3.11")
	if !ok {
		t.Fatal("python-3.11 no longer resolves")
	}
	got := res["python-3.11"]
	if got.VCPUs != ci.CPU || got.MemoryMB != ci.MemoryMB || got.StorageMB != ci.StorageMB {
		t.Errorf("python-3.11 defaults = %+v, want its entry's %d vCPU / %d MB / %d MB",
			got, ci.CPU, ci.MemoryMB, ci.StorageMB)
	}

	// A disk manifest for the alias id wins over the entry it aliases.
	own := templates.Template{ID: "python-3.11", CPU: 4, MemoryMB: 4096, StorageMB: 8192}
	if got := templateResources(append(cat, own))["python-3.11"]; got.VCPUs != 4 || got.StorageMB != 8192 {
		t.Errorf("a disk manifest for the alias was overridden: %+v", got)
	}
}
