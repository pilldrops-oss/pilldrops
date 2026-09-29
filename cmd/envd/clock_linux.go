//go:build linux

package main

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// setGuestClock steps CLOCK_REALTIME to unixNS.
//
// A guest restored from a memory image comes back believing the wall clock is
// whatever it was when the snapshot was written — measured on the reference
// host, a 62-second pause left the guest's `date` 62 seconds behind. Nothing in
// the guest notices: there is no NTP, no virtio-rtc and no hypervisor time
// device wired up. Everything that checks a certificate's validity, signs a
// request or writes a timestamp is then wrong by however long the sandbox was
// paused, which for an overnight pause is hours.
//
// Stepping rather than slewing: adjtime would take longer than the drift to
// correct, and nothing here depends on monotonicity of the wall clock.
func setGuestClock(unixNS int64) error {
	ts := unix.NsecToTimespec(unixNS)
	if err := unix.ClockSettime(unix.CLOCK_REALTIME, &ts); err != nil {
		return fmt.Errorf("clock_settime: %w", err)
	}
	return nil
}
