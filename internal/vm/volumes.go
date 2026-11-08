package vm

import (
	"fmt"
	"path/filepath"
	"regexp"
)

// MinVolumeMB and MaxVolumeMB bound a volume's size: mkfs.ext4's floor, and the
// largest disk any plan grants a sandbox.
const (
	MinVolumeMB = minVolumeMB
	MaxVolumeMB = 100 * 1024
)

const minVolumeMB = 256

// volumeIDRe is the shape the API mints; anything else could name a path
// outside the volume directory.
var volumeIDRe = regexp.MustCompile(`^vol-[a-z0-9]{8,40}$`)

// VolumePath is where a volume's image lives on this node: beside the
// overlays, in their parent's "volumes" directory.
func (m *Manager) VolumePath(id string) (string, error) {
	if !volumeIDRe.MatchString(id) {
		return "", fmt.Errorf("invalid volume id %q", id)
	}
	return filepath.Join(filepath.Dir(m.config.OverlayDir), "volumes", id+".ext4"), nil
}
