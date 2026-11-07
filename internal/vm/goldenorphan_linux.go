package vm

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// goldensInFlight is the golden VMs this process is building right now. NewManager
// starts builds before its orphan scan, and a running build must not read as dead.
var goldensInFlight sync.Map

func goldenBuildStarted(id string)  { goldensInFlight.Store(id, struct{}{}) }
func goldenBuildFinished(id string) { goldensInFlight.Delete(id) }

// isOrphanedGolden reports whether id is a golden VM left by a dead build of this
// daemon. Its overlay in this daemon's directory is the ownership record: another
// daemon on the host builds in its own, and that VM must be left alone.
func (m *Manager) isOrphanedGolden(id string) bool {
	if !strings.HasPrefix(id, "golden-") {
		return false
	}
	if _, building := goldensInFlight.Load(id); building {
		return false
	}
	_, err := os.Stat(filepath.Join(m.config.OverlayDir, id+".ext4"))
	return err == nil
}
