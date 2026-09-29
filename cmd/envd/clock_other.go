//go:build !linux

package main

import "errors"

// setGuestClock refuses off Linux. A stub that returned nil would let the node
// believe a resumed guest's clock had been corrected when nothing touched it.
func setGuestClock(int64) error {
	return errors.New("this build cannot set the guest clock")
}
