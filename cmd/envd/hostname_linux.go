//go:build linux

package main

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// setHostname renames the running kernel.
func setHostname(name string) error {
	if err := unix.Sethostname([]byte(name)); err != nil {
		return fmt.Errorf("sethostname: %w", err)
	}
	return nil
}
