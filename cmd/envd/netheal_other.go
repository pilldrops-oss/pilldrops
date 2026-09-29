//go:build !linux

package main

import "fmt"

func startNetHeal() {}

// applyGuestNetwork refuses off Linux; a stub that said OK would report a sandbox ready on an address nothing holds.
func applyGuestNetwork(c netConfig) error {
	return fmt.Errorf("this build cannot reconfigure a guest interface (%s)", c.Device)
}
