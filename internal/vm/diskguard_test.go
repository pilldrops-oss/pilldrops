//go:build linux

package vm

import (
	"context"
	"errors"
	"testing"
)

// The disk watchdog covered Create, ForkSandbox and CheckpointSandbox, and not
// the path a named snapshot boots through — which is the single largest write
// the node makes, since it copies the memory image, the machine state and the
// overlay disk before anything boots. On a full filesystem that create was
// accepted and died part-way through the copy with a raw ENOSPC.
//
// Both directions, because a guard stuck on is as wrong as one stuck off: with
// the flag set the boot must be refused before it reads anything, and with it
// clear the same call must get past the guard and fail for its own reason.
func TestABootFromACaptureIsRefusedUnderDiskPressure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		call func(m *Manager) error
	}{
		{"BootFromCapture", func(m *Manager) error {
			_, err := m.BootFromCapture(context.Background(), t.TempDir(), "sb-test", 1, 512, nil)
			return err
		}},
		{"ForkFromSnapshot", func(m *Manager) error {
			_, err := m.ForkFromSnapshot(context.Background(), t.TempDir(), "sb-test", 1, 512)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manager{config: Config{NetNS: true}}

			m.SetDiskPressure(true)
			if err := tc.call(m); !errors.Is(err, ErrDiskPressure) {
				t.Fatalf("%s on a full filesystem returned %v, want ErrDiskPressure — the node "+
					"would start the copy and run out of space mid-write", tc.name, err)
			}

			// Positive control: the refusal has to be the watchdog and not this
			// empty directory, so the same call with the flag clear must get
			// past the guard and fail on the missing capture instead.
			m.SetDiskPressure(false)
			err := tc.call(m)
			if err == nil {
				t.Fatalf("%s booted from an empty directory", tc.name)
			}
			if errors.Is(err, ErrDiskPressure) {
				t.Fatalf("%s still reports disk pressure with the flag cleared: the guard is "+
					"stuck on and no sandbox could ever be restored", tc.name)
			}
		})
	}
}
