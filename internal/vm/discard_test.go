//go:build linux

package vm

import (
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
)

// DiscardSnapshot releases everything a pause kept from meta.json alone: the
// overlay, the lineage dir and the records under the id the sandbox was paused
// as. A missing snapshot dir is not an error (a never-paused sandbox).
func TestDiscardSnapshotReleasesOverlayLineageAndRecords(t *testing.T) {
	root := t.TempDir()
	m := &Manager{config: Config{OverlayDir: filepath.Join(root, "overlays"), NetNS: true}, active: map[string]*Sandbox{}, log: zap.NewNop()}
	const id = "sb-1-vmid"
	overlay := filepath.Join(m.config.OverlayDir, id+".ext4")
	lineage := m.lineageDir(id)
	for _, d := range []string{m.config.OverlayDir, lineage, m.tokenDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{overlay, filepath.Join(lineage, "mem.1"), m.tokenPath(id), m.endpointPath(id), m.tapPath(id)} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	snap := filepath.Join(root, id+".snap")
	if err := os.MkdirAll(snap, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeSnapMeta(snap, sandboxSnapMeta{ID: id, OverlayPath: overlay, VMIP: "192.168.1.2", TapName: "tap-x"}); err != nil {
		t.Fatal(err)
	}

	if err := m.DiscardSnapshot(snap); err != nil {
		t.Fatalf("DiscardSnapshot: %v", err)
	}
	for _, p := range []string{overlay, lineage, m.tokenPath(id), m.endpointPath(id), m.tapPath(id)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still exists after discard (err=%v)", p, err)
		}
	}
	// The snapshot dir itself is the daemon's file, left for it to remove.
	if _, err := os.Stat(filepath.Join(snap, "meta.json")); err != nil {
		t.Errorf("meta.json removed by discard; the daemon owns that dir: %v", err)
	}
	if err := m.DiscardSnapshot(filepath.Join(root, "never-paused.snap")); err != nil {
		t.Errorf("missing snapshot dir must not be an error: %v", err)
	}
}
