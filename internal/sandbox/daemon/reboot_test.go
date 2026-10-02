package daemon

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/runtimetest"
)

// rebootRT is a runtime after a host reboot: every container still exists,
// stopped, and Adopt hands each back as a casualty to start again.
type rebootRT struct {
	*runtimetest.Fake
	stopped map[string]*casualtyVMM
	swept   [][]string
}

func (r *rebootRT) Adopt(_ context.Context, id string) (rt.Instance, bool) {
	c, ok := r.stopped[id]
	return c, ok
}

func (r *rebootRT) SweepUnknown(known []string) { r.swept = append(r.swept, known) }

// rebooted builds a runtime holding one stopped container per id; ids in
// wontStart fail their start.
func rebooted(t *testing.T, ids []string, wontStart map[string]bool) *rebootRT {
	t.Helper()
	r := &rebootRT{Fake: runtimetest.NewFake(), stopped: map[string]*casualtyVMM{}}
	for _, id := range ids {
		inst, err := r.Boot(context.Background(), rt.BootSpec{SandboxID: id})
		if err != nil {
			t.Fatal(err)
		}
		c := &casualtyVMM{deadVMM: deadVMM{inst}, revived: inst}
		if wontStart[id] {
			c.revived, c.reviveErr = nil, errors.New("docker start: exit 1")
		}
		r.stopped[id] = c
	}
	return r
}

func TestRebootRestoresEveryStoppedContainerAndRemovesNothing(t *testing.T) {
	standby := runningRec("sb-standby", time.Now().Add(time.Hour))
	standby.State = string(StateStandby)
	// Paused, resumed, then rebooted: the record still names the old snapshot.
	resumed := runningRec("sb-resumed", time.Now().Add(time.Hour))
	resumed.Snapshot = "/snap/sb-resumed"
	failed := runningRec("sb-failed", time.Now().Add(time.Hour))
	failed.State = string(StateFailed)
	recs := []PersistedSandbox{runningRec("sb-a", time.Now().Add(time.Hour)), standby, resumed, failed}
	ids := []string{"sb-a", "sb-standby", "sb-resumed", "sb-failed"}

	r := rebooted(t, ids, nil)
	fp := newFakePersister(recs)
	d := New(Config{Runtime: r, Store: fp, SnapshotDir: t.TempDir(), IdleTimeout: time.Hour, HardTTL: time.Hour})
	d.Start()
	defer d.Stop()

	for _, id := range ids {
		info, ok := d.Get(id)
		if !ok {
			t.Errorf("%s: dropped after the reboot; its container held its data", id)
			continue
		}
		if info.State != StateRunning {
			t.Errorf("%s: state = %s, want running", id, info.State)
		}
		if r.stopped[id].stopped {
			t.Errorf("%s: its container was removed", id)
		}
		if fp.deleted[id] {
			t.Errorf("%s: its record was deleted", id)
		}
	}
	if fp.countEvents("restarted") != len(ids) {
		t.Errorf("events = %v, want one restarted per sandbox", fp.events)
	}
	if len(r.swept) != 1 || len(r.swept[0]) != len(ids) {
		t.Fatalf("sweep known sets = %v, want all %d sandboxes known", r.swept, len(ids))
	}
}

func TestRebootKeepsAContainerThatWillNotStartAsFailed(t *testing.T) {
	r := rebooted(t, []string{"sb-broken"}, map[string]bool{"sb-broken": true})
	fp := newFakePersister([]PersistedSandbox{runningRec("sb-broken", time.Now().Add(time.Hour))})
	d := New(Config{Runtime: r, Store: fp, SnapshotDir: t.TempDir(), IdleTimeout: time.Hour, HardTTL: time.Hour})
	d.Start()
	defer d.Stop()

	info, ok := d.Get("sb-broken")
	if !ok {
		t.Fatal("a container that would not start had its sandbox dropped")
	}
	if info.State != StateFailed {
		t.Errorf("state = %s, want failed", info.State)
	}
	if r.stopped["sb-broken"].stopped || fp.deleted["sb-broken"] {
		t.Error("the container or its record was removed")
	}
	if got := fp.saved["sb-broken"].State; got != string(StateFailed) {
		t.Errorf("persisted state = %q, want failed", got)
	}
	if fp.countEvents("exited") != 1 {
		t.Errorf("events = %v, want one exited", fp.events)
	}
	if len(r.swept) != 1 || !slices.Contains(r.swept[0], "sb-broken") {
		t.Errorf("sweep known sets = %v; the failed sandbox must be known, or its container goes", r.swept)
	}
}
