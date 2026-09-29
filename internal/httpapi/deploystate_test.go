package httpapi

import (
	"testing"
	"time"
)

func TestDeployTrackerReportsProgressAsItHappens(t *testing.T) {
	tr := newDeployTracker()
	tr.start("sb-1")

	p, known := tr.get("sb-1")
	if !known || p.State != DeployRunning {
		t.Fatalf("a started deploy must read as running, got known=%v state=%q", known, p.State)
	}

	tr.step("sb-1", DeployStep{Cmd: "npm install", MS: 4200}, "setup 'npm install' ok")
	p, _ = tr.get("sb-1")
	if len(p.Steps) != 1 || p.Steps[0].MS != 4200 {
		t.Fatalf("step not recorded: %+v", p.Steps)
	}
	if p.State != DeployRunning {
		t.Fatalf("still running after one step, got %q", p.State)
	}

	tr.finish("sb-1", DeployDone)
	if p, _ := tr.get("sb-1"); p.State != DeployDone {
		t.Fatalf("finish did not settle the state: %q", p.State)
	}
}

func TestDeployTrackerDistinguishesUnknownFromDone(t *testing.T) {
	tr := newDeployTracker()
	if _, known := tr.get("never-seen"); known {
		t.Fatal("an unknown sandbox must not be reported as a known deploy")
	}
}

func TestDeployTrackerCopiesSoCallersCannotMutateIt(t *testing.T) {
	tr := newDeployTracker()
	tr.start("sb-1")
	tr.step("sb-1", DeployStep{Cmd: "a"}, "a ok")

	p, _ := tr.get("sb-1")
	p.Steps[0].Cmd = "mutated"
	p.Log[0] = "mutated"

	fresh, _ := tr.get("sb-1")
	if fresh.Steps[0].Cmd != "a" || fresh.Log[0] != "a ok" {
		t.Fatalf("caller mutated tracked state: %+v %v", fresh.Steps, fresh.Log)
	}
}

func TestDeployTrackerEvictsFinishedDeploys(t *testing.T) {
	tr := newDeployTracker()
	for i := 0; i < 300; i++ {
		id := string(rune('a'+i%26)) + time.Now().Format("150405.000000000") + string(rune(i))
		tr.start(id)
		tr.finish(id, DeployDone)
		tr.mu.Lock()
		tr.byID[id].at = time.Now().Add(-time.Hour)
		tr.mu.Unlock()
	}
	tr.start("fresh")
	tr.mu.RLock()
	n := len(tr.byID)
	tr.mu.RUnlock()
	if n >= 300 {
		t.Fatalf("nothing was evicted: %d entries", n)
	}
}

func TestDeployTrackerKeepsRunningDeploysWhateverTheirAge(t *testing.T) {
	tr := newDeployTracker()
	tr.start("slow")
	tr.mu.Lock()
	tr.byID["slow"].at = time.Now().Add(-24 * time.Hour)
	for i := 0; i < 300; i++ {
		id := "filler" + string(rune(i))
		tr.byID[id] = &trackedDeploy{progress: DeployProgress{State: DeployDone}, at: time.Now().Add(-time.Hour)}
	}
	tr.evictLocked()
	tr.mu.Unlock()

	if _, known := tr.get("slow"); !known {
		t.Fatal("an in-flight deploy was evicted")
	}
}
