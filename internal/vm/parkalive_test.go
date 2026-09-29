//go:build linux

package vm

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// parkMachine fails Stop and reports a pid the test controls, so the two cases
// that matter can be told apart: a VMM that is gone (Stop errors, harmless) and
// one that is still running (Stop errors, and parking would be destructive).
type parkMachine struct {
	FirecrackerMachine
	pid int
}

func (p *parkMachine) Stop(context.Context) error { return errors.New("vmm did not stop") }
func (p *parkMachine) Pid() int                   { return p.pid }

func newParkManager(t *testing.T, pid int) *Manager {
	t.Helper()
	return &Manager{
		config: Config{OverlayDir: t.TempDir()},
		active: map[string]*Sandbox{
			// stopTimer is the TTL cancel a real create installs; ParkSandbox
			// calls it unconditionally.
			"sb-park": {ID: "sb-park", Machine: &parkMachine{pid: pid}, stopTimer: func() {}},
		},
		log: zap.NewNop(),
	}
}

// RestoreCheckpoint overwrites this sandbox's overlay and boots from it the
// moment ParkSandbox returns nil. A VMM still holding that overlay would have
// its disk rewritten underneath it and then be joined by a second VMM, so the
// park has to refuse rather than log and continue.
func TestParkRefusesWhileTheVMMIsStillAlive(t *testing.T) {
	m := newParkManager(t, os.Getpid()) // our own pid is certainly alive

	err := m.ParkSandbox(context.Background(), "sb-park")
	if err == nil {
		t.Fatal("park returned nil with the VMM still running; RestoreCheckpoint would now overwrite a live overlay and boot a second VMM over it")
	}
	if !strings.Contains(err.Error(), "still running") {
		t.Errorf("the refusal must say why, got %q", err)
	}
}

// The common case must keep working: Stop reports an error for a process that
// has already exited, and that park is safe because nothing holds the overlay.
func TestParkProceedsWhenTheVMMIsAlreadyGone(t *testing.T) {
	// A pid that cannot be signalled stands in for a VMM that has exited.
	m := newParkManager(t, 0x7FFFFFFE)

	if err := m.ParkSandbox(context.Background(), "sb-park"); err != nil {
		t.Fatalf("park refused a dead VMM, which would make every rollback after a noisy Stop fail: %v", err)
	}
}
