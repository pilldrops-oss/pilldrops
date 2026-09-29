//go:build linux

package vm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
)

// abortMachine counts the freeze and thaw a failed pause is supposed to leave
// balanced, and fails Stop so a test that reached it would say so.
type abortMachine struct {
	FirecrackerMachine
	paused, resumed, stopped int
}

func (a *abortMachine) PauseVM(context.Context) error  { a.paused++; return nil }
func (a *abortMachine) ResumeVM(context.Context) error { a.resumed++; return nil }
func (a *abortMachine) Stop(context.Context) error     { a.stopped++; return nil }
func (a *abortMachine) ChrootRoot() string             { return "" }

// A pause that cannot write its image must give the sandbox back. Before this,
// SnapshotSandbox dropped the machine from the active set, closed envd and
// cancelled its timer before the first thing that could fail — so a snapshot
// error left a frozen VM nobody could reach, while the daemon reverted its
// record to "running" and went on billing for it. `exec` answered "sandbox not
// found", `sandbox get` answered "running", and neither was wrong about what it
// could see.
func TestAFailedPauseGivesTheSandboxBack(t *testing.T) {
	mach := &abortMachine{}
	timerCancelled := false
	m := &Manager{
		config: Config{OverlayDir: t.TempDir()},
		log:    zap.NewNop(),
		active: map[string]*Sandbox{
			"sb-1": {ID: "sb-1", Machine: mach, stopTimer: func() { timerCancelled = true }},
		},
	}

	// A destination the snapshot cannot be written to: a path under a regular
	// file, so the very first step fails and nothing has to be mocked deeper.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	err := m.SnapshotSandbox(context.Background(), "sb-1", filepath.Join(blocker, "snap"))
	if err == nil {
		t.Fatal("pause reported success with no snapshot written")
	}

	if _, ok := m.active["sb-1"]; !ok {
		t.Error("the sandbox is gone from the active set after a pause that failed; " +
			"the record still calls it running and nothing can reach it")
	}
	if mach.resumed != 1 {
		t.Errorf("guest resumed %d times, want 1: a failed pause leaves it frozen", mach.resumed)
	}
	if mach.stopped != 0 {
		t.Error("the VMM was stopped for a snapshot that was never written")
	}
	if timerCancelled {
		t.Error("the sandbox's TTL timer was cancelled by a pause that failed")
	}
}

// The dirty bitmap is reset by the capture itself, so an aborted pause must not
// leave the old image on the sandbox: the next capture would diff against a
// bitmap that no longer covers it and write a memory image missing the pages
// dirtied before the failure — a snapshot that restores into a corrupt guest
// and reports success.
func TestAnAbortedCaptureForcesTheNextOneFull(t *testing.T) {
	m := &Manager{config: Config{OverlayDir: t.TempDir()}, log: zap.NewNop()}
	sb := &Sandbox{ID: "sb-1", Machine: &failDiffMachine{}, MemBase: "/gone/mem"}

	if _, err := m.beginMemorySnapshot(context.Background(), sb, t.TempDir()); err == nil {
		t.Fatal("capture reported success")
	}
	if sb.MemBase != "" {
		t.Errorf("memory base still %q after a capture that failed; the next diff "+
			"would be merged onto an image the reset bitmap no longer describes", sb.MemBase)
	}
}

type failDiffMachine struct {
	FirecrackerMachine
}

func (f *failDiffMachine) ChrootRoot() string { return "" }
func (f *failDiffMachine) CreateDiffSnapshot(context.Context, string, string) error {
	return errors.New("no space left on device")
}
