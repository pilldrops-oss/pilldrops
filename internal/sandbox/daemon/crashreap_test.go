package daemon

import (
	"context"
	"errors"
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

// slowOOM is a guest whose OOM probe takes the whole scan budget.
type slowOOM struct{ rt.Instance }

func (slowOOM) OOMKillCount(ctx context.Context) (int, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}

func TestExitScanRunsWhenTheOtherScannersEatTheBudget(t *testing.T) {
	old := scanBudget
	scanBudget = 200 * time.Millisecond
	t.Cleanup(func() { scanBudget = old })

	fp := newFakePersister(nil)
	d := New(Config{Runtime: runtimetest.NewFake(), Store: fp, IdleTimeout: time.Hour, HardTTL: time.Hour})
	ctx := context.Background()
	slow, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	dead, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	d.mu.Lock()
	d.boxes[slow.ID].inst = slowOOM{d.boxes[slow.ID].inst}
	d.boxes[dead.ID].inst = deadVMM{d.boxes[dead.ID].inst}
	d.mu.Unlock()

	d.scanOnce(ctx)

	if _, ok := d.Get(dead.ID); ok {
		t.Fatal("the dead sandbox was not reaped: the exit scan got no budget")
	}
}

// casualtyVMM is an instance a dockerd restart stopped.
type casualtyVMM struct {
	deadVMM
	revived   rt.Instance
	reviveErr error
	stopped   bool
}

func (*casualtyVMM) Casualty() bool { return true }
func (c *casualtyVMM) Revive(context.Context) (rt.Instance, error) {
	return c.revived, c.reviveErr
}
func (c *casualtyVMM) Stop(context.Context) error { c.stopped = true; return nil }

func TestScanRevivesARestartCasualtyAndKeepsOneThatWillNotStart(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reviveErr error
		state     State
		event     string
	}{
		{"started again", nil, StateRunning, "restarted"},
		{"will not start", errors.New("no such container"), StateFailed, "exited"},
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
			c := &casualtyVMM{deadVMM: deadVMM{orig}, revived: orig, reviveErr: tc.reviveErr}
			d.boxes[sb.ID].inst = c
			d.mu.Unlock()

			d.scanExits(ctx)

			info, ok := d.Get(sb.ID)
			if !ok {
				t.Fatal("a restart casualty was reaped; its data is gone")
			}
			if info.State != tc.state {
				t.Errorf("state = %q, want %q", info.State, tc.state)
			}
			if c.stopped {
				t.Error("the casualty's container was stopped, which removes it")
			}
			if fp.countEvents(tc.event) != 1 {
				t.Errorf("events = %v, want one %q", fp.events, tc.event)
			}
			if _, ok := fp.history[sb.ID]; ok {
				t.Error("a kept sandbox went to history")
			}
			if got := fp.saved[sb.ID].State; got != string(tc.state) {
				t.Errorf("persisted state = %q, want %q", got, tc.state)
			}
			d.mu.Lock()
			swapped := d.boxes[sb.ID].inst == orig
			d.mu.Unlock()
			if tc.reviveErr == nil && !swapped {
				t.Error("the revived instance did not replace the casualty")
			}
		})
	}
}

// envdDownVMM is a running instance nothing can exec into: its envd is down.
type envdDownVMM struct {
	rt.Instance
	down bool
}

// Freeze and Thaw make it a standby-capable instance, as both real runtimes are.
func (*envdDownVMM) Freeze(context.Context) error { return nil }
func (*envdDownVMM) Thaw(context.Context) error   { return nil }

func (e *envdDownVMM) OOMKillCount(context.Context) (int, error) {
	if e.down {
		return 0, errors.New("envd: connection refused")
	}
	return 0, nil
}

func TestScanReportsASandboxItCannotExecIntoOnce(t *testing.T) {
	fp := newFakePersister(nil)
	d := New(Config{Runtime: runtimetest.NewFake(), Store: fp, IdleTimeout: time.Hour, HardTTL: time.Hour})
	ctx := context.Background()
	sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	d.mu.Lock()
	e := &envdDownVMM{Instance: d.boxes[sb.ID].inst, down: true}
	d.boxes[sb.ID].inst = e
	d.mu.Unlock()

	for range unreachableAfter - 1 {
		d.scanOOMKills(ctx)
	}
	if n := fp.countEvents("unreachable"); n != 0 {
		t.Fatalf("reported after %d failed probes; a busy guest can miss one", unreachableAfter-1)
	}
	for range 3 {
		d.scanOOMKills(ctx)
	}
	if n := fp.countEvents("unreachable"); n != 1 {
		t.Fatalf("unreachable events = %d, want exactly 1 while envd stays down", n)
	}

	e.down = false
	d.scanOOMKills(ctx)
	e.down = true
	for range unreachableAfter {
		d.scanOOMKills(ctx)
	}
	if n := fp.countEvents("unreachable"); n != 2 {
		t.Errorf("unreachable events = %d, want a second one after envd answered and went down again", n)
	}
}

// A failed probe from before a pause, resume or freeze is not a failure of the
// sandbox that comes back: the count starts again with the state.
func TestUnreachableCountStartsAgainAcrossATransition(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		transition func(d *Daemon, id string) error
	}{
		{"pause and resume", func(d *Daemon, id string) error {
			if err := d.Pause(ctx, id); err != nil {
				return err
			}
			return d.Resume(ctx, id)
		}},
		{"standby and wake", func(d *Daemon, id string) error {
			if _, err := d.Standby(ctx, id); err != nil {
				return err
			}
			return d.Resume(ctx, id)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fp := newFakePersister(nil)
			d := New(Config{Runtime: runtimetest.NewFake(), Store: fp, SnapshotDir: t.TempDir(), IdleTimeout: time.Hour, HardTTL: time.Hour})
			sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			// Wraps the current instance unless a transition kept the wrapped one.
			envdDown := func() {
				d.mu.Lock()
				defer d.mu.Unlock()
				if _, ok := d.boxes[sb.ID].inst.(*envdDownVMM); !ok {
					d.boxes[sb.ID].inst = &envdDownVMM{Instance: d.boxes[sb.ID].inst, down: true}
				}
			}
			envdDown()
			for range unreachableAfter - 1 {
				d.scanOOMKills(ctx)
			}
			if err := tc.transition(d, sb.ID); err != nil {
				t.Fatal(err)
			}
			envdDown()
			d.scanOOMKills(ctx)
			if fp.countEvents("unreachable") != 0 {
				t.Errorf("reported after one failed probe since the %s; failures from before it carried over", tc.name)
			}
		})
	}
}
