//go:build linux

package vm

import (
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
)

// A sandboxd sharing a host must never destroy what its own state dir has no record of.

// ownershipManager builds a bare Manager; NewManager would run the sweeps under test.
func ownershipManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	return &Manager{
		config: Config{OverlayDir: filepath.Join(dir, "overlays")},
		active: map[string]*Sandbox{},
		log:    zap.NewNop(),
	}
}

func TestRecoverOrphansSparesUnownedVMs(t *testing.T) {
	m := ownershipManager(t)

	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	old := jailerBaseDir
	jailerBaseDir = t.TempDir()
	defer func() { jailerBaseDir = old }()

	m.recoverOrphanProcs([]*OrphanProcess{{
		SandboxID: "sb-neighbours-vm",
		Machine:   &procKiller{pid: pid},
	}})

	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatal("RecoverOrphans killed a process it has NO state-dir record of.\n\n" +
			"On a host with two sandboxds that process is the other daemon's live VM: " +
			"measured pre-fix, a booting side-car SIGKILLed its neighbour's four healthy " +
			"VMMs inside a second. No record → not ours → hands off.")
	}
}

func TestRecoverOrphansStillReapsOwnedVMs(t *testing.T) {
	m := ownershipManager(t)

	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	reaped := make(chan struct{})
	go func() { _, _ = cmd.Process.Wait(); close(reaped) }()

	old := jailerBaseDir
	jailerBaseDir = t.TempDir()
	defer func() { jailerBaseDir = old }()

	const id = "sb-our-own-orphan"
	// 127.0.0.1:1 answers nothing, so the liveness dial fails fast.
	m.saveEndpoint(id, "127.0.0.1:1")
	m.recoverOrphanProcs([]*OrphanProcess{{SandboxID: id, Machine: &procKiller{pid: pid}}})

	select {
	case <-reaped:
	case <-time.After(10 * time.Second):
		t.Fatal("an orphan WITH a state-dir record was not reaped — the ownership scoping broke recovery itself")
	}
	if m.loadEndpoint(id) != "" {
		t.Fatal("reaped orphan's endpoint record was not removed")
	}
}

func TestStaleTapDecisionNeverTouchesUnrecordedTaps(t *testing.T) {
	records := map[string]string{
		"sb-dead":  "fctap7", // no longer active, victim
		"sb-alive": "fctap8", // still active, spared
	}
	active := map[string]bool{"sb-alive": true}

	victims := staleTapDecision(records, active)
	if len(victims) != 1 || victims[0].tap != "fctap7" {
		t.Fatalf("victims = %v, want exactly fctap7 (sb-dead)", victims)
	}
	for _, v := range victims {
		if _, ok := records[v.id]; !ok {
			t.Fatalf("decision produced a victim outside its own records: %v", v)
		}
	}
}
