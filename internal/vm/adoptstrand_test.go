//go:build linux

package vm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
)

type orphanMachine struct{ abortMachine }

func (*orphanMachine) Pid() int { return 0 }

// A VM that failed adoption used to take its disk with it, leaving a record that
// came back "paused" and could never resume.
func TestFailedAdoptionKeepsTheDiskUntilTheSweep(t *testing.T) {
	root := t.TempDir()
	m := &Manager{
		config:      Config{OverlayDir: filepath.Join(root, "overlays")},
		log:         zap.NewNop(),
		active:      map[string]*Sandbox{},
		recoverable: map[string]*OrphanProcess{},
		stranded:    map[string]string{},
	}
	if err := os.MkdirAll(m.tokenDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	strand := func(owner string) string {
		overlay := filepath.Join(root, owner+".ext4")
		if err := os.WriteFile(overlay, []byte("disk"), 0o600); err != nil {
			t.Fatal(err)
		}
		// Nothing listens here, so envd cannot answer.
		record, err := json.Marshal(adoptRecord{Owner: owner, VMID: owner, OverlayPath: overlay, NodeAddr: "127.0.0.1:1"})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(m.adoptPath(owner), record, 0o600); err != nil {
			t.Fatal(err)
		}
		m.recoverable[owner] = &OrphanProcess{SandboxID: owner, Machine: &orphanMachine{}}
		if _, adopted := m.AdoptOwned(owner); adopted {
			t.Fatalf("%s adopted with no envd", owner)
		}
		return overlay
	}
	claimed, unclaimed := strand("sb-claimed"), strand("sb-unclaimed")
	for _, overlay := range []string{claimed, unclaimed} {
		if _, err := os.Stat(overlay); err != nil {
			t.Errorf("disk gone after a failed adoption: %v", err)
		}
	}

	m.KillUnadopted([]string{"sb-claimed"})

	if _, err := os.Stat(claimed); err != nil {
		t.Errorf("the sweep removed a disk its record still needs: %v", err)
	}
	if _, err := os.Stat(unclaimed); !os.IsNotExist(err) {
		t.Errorf("a disk no record claims survived the sweep: %v", err)
	}
}
