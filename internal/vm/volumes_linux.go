//go:build linux

package vm

import (
	"fmt"
	"os"
	"path/filepath"
)

// CreateVolume makes an empty ext4 volume of sizeMB on this node. It is owned
// by root until a sandbox's VMM takes it at attach.
func (m *Manager) CreateVolume(id string, sizeMB int) error {
	if sizeMB < MinVolumeMB || sizeMB > MaxVolumeMB {
		return fmt.Errorf("size_mb must be %d-%d", MinVolumeMB, MaxVolumeMB)
	}
	path, err := m.VolumePath(id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("volume %s already exists", id)
	}
	return makeExt4(path, 0, sizeMB)
}

// DeleteVolume removes a volume's image. A missing one is not an error.
func (m *Manager) DeleteVolume(id string) error {
	path, err := m.VolumePath(id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
