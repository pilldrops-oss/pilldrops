package daemon

import (
	"context"
	"testing"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/runtimetest"
)

// deadVMM is an instance whose VMM process reports as gone.
type deadVMM struct{ rt.Instance }

func (deadVMM) Alive() bool { return false }

// oomVMM is a dead instance its runtime saw killed for memory.
type oomVMM struct{ deadVMM }

func (oomVMM) OOMKilled() bool { return true }

func TestScanReapsADeadInstanceAndSaysWhy(t *testing.T) {
	for _, tc := range []struct {
		name string
		wrap func(rt.Instance) rt.Instance
		want string
	}{
		{"exited", func(i rt.Instance) rt.Instance { return deadVMM{i} }, "exited"},
		{"oom", func(i rt.Instance) rt.Instance { return oomVMM{deadVMM{i}} }, "oom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fp := newFakePersister(nil)
			d := New(Config{Runtime: runtimetest.NewFake(), Store: fp, IdleTimeout: time.Hour, HardTTL: time.Hour})
			ctx := context.Background()
			sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			d.mu.Lock()
			d.boxes[sb.ID].inst = tc.wrap(d.boxes[sb.ID].inst)
			d.mu.Unlock()

			d.scanExits(ctx)

			if _, ok := d.Get(sb.ID); ok {
				t.Fatal("a sandbox whose instance died still lists as running")
			}
			if n := fp.countEvents(tc.want); n != 1 {
				t.Errorf("%q events = %d, want 1 (events %v)", tc.want, n, fp.events)
			}
			if got := fp.history[sb.ID].ExitState; got != tc.want {
				t.Errorf("history exit state = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestScanKeepsLiveSandbox(t *testing.T) {
	d, _ := newTestDaemon(t)
	ctx := context.Background()
	sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	d.scanExits(ctx)
	d.reconcileOnce(ctx)
	if _, ok := d.Get(sb.ID); !ok {
		t.Fatalf("live sandbox was wrongly reaped")
	}
}

// racingVMM reads dead once, running onFirstProbe in between the scan's
// lock-free probe and its destroy, the window a pause or restore can fill.
type racingVMM struct {
	rt.Instance
	onFirstProbe func()
	probes       int
	aliveAfter   bool
}

func (r *racingVMM) Alive() bool {
	r.probes++
	if r.probes == 1 {
		if r.onFirstProbe != nil {
			r.onFirstProbe()
		}
		return false
	}
	return r.aliveAfter
}

func TestScanDoesNotReapASandboxThatChangedUnderneathIt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(d *Daemon, id string, orig rt.Instance, r *racingVMM)
	}{
		{"paused", func(d *Daemon, id string, _ rt.Instance, r *racingVMM) {
			r.onFirstProbe = func() {
				if err := d.Pause(context.Background(), id); err != nil {
					t.Errorf("pause: %v", err)
				}
			}
		}},
		{"instance swapped by a restore", func(d *Daemon, id string, orig rt.Instance, r *racingVMM) {
			r.onFirstProbe = func() {
				d.mu.Lock()
				d.boxes[id].inst = orig
				d.mu.Unlock()
			}
		}},
		{"answered on the second probe", func(_ *Daemon, _ string, _ rt.Instance, r *racingVMM) {
			r.aliveAfter = true
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fp := newFakePersister(nil)
			d := New(Config{Runtime: runtimetest.NewFake(), Store: fp, IdleTimeout: time.Hour, HardTTL: time.Hour})
			ctx := context.Background()
			sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			d.mu.Lock()
			orig := d.boxes[sb.ID].inst
			r := &racingVMM{Instance: orig}
			d.boxes[sb.ID].inst = r
			d.mu.Unlock()
			tc.setup(d, sb.ID, orig, r)

			d.scanExits(ctx)

			if _, ok := d.Get(sb.ID); !ok {
				t.Fatal("a healthy sandbox was destroyed by the exit scan")
			}
			if n := fp.countEvents("exited"); n != 0 {
				t.Errorf("exited events = %d, want 0", n)
			}
		})
	}
}
