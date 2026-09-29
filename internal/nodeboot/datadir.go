package nodeboot

import (
	"fmt"
	"os"
)

// DataDir creates the node's data directory, or narrows an existing one, to
// 0700. It holds guest memory images, machine state and envd tokens; files
// under it are written 0644 by default, and a traversable parent made every
// sandbox's memory readable by any local user of the host. Jailed VMMs reach
// their files through hard links in their chroot, never by this path.
func DataDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("data dir %q could not be created: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("data dir %q could not be made private: %w", dir, err)
	}
	return nil
}
