package daemon

import (
	"context"
	"errors"
	"fmt"
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
	revives   int
	engine    time.Time
}

func (*casualtyVMM) Casualty() bool             { return true }
func (c *casualtyVMM) EngineStarted() time.Time { return c.engine }
func (c *casualtyVMM) Revive(context.Context) (rt.Instance, error) {
	c.revives++
	return c.revived, c.reviveErr
}
func (c *casualtyVMM) Stop(context.Context) error { c.stopped = true; return nil }

func TestScanRevivesARestartCasualty(t *testing.T) {
	fp := newFakePersister(nil)
	d := New(Config{Runtime: runtimetest.NewFake(), Store: fp, IdleTimeout: time.Hour, HardTTL: time.Hour})
	ctx := context.Background()
	sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	d.mu.Lock()
	orig := d.boxes[sb.ID].inst
	c := &casualtyVMM{deadVMM: deadVMM{orig}, revived: orig}
	d.boxes[sb.ID].inst = c
	d.mu.Unlock()

	d.scanExits(ctx)

	info, ok := d.Get(sb.ID)
	if !ok || info.State != StateRunning {
		t.Fatalf("a restart casualty that came back: present %v, state %q; want running", ok, info.State)
	}
	if c.stopped {
		t.Error("the casualty's container was stopped, which removes it")
	}
	if fp.countEvents("restarted") != 1 {
		t.Errorf("events = %v, want one restarted", fp.events)
	}
	d.mu.Lock()
	swapped := d.boxes[sb.ID].inst == orig
	d.mu.Unlock()
	if !swapped {
		t.Error("the revived instance did not replace the casualty")
	}
}

// A crashed sandbox is destroyed, the way E2B does it: one whose start fails,
// or whose container starts and dies again every scan, must not loop.
func TestScanDestroysACasualtyThatWillNotStayUp(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reviveErr error
		restarted int
	}{
		{"confirmed stopped after its start", fmt.Errorf("docker start c1: %w", rt.ErrWontStayUp), 0},
		{"starts and dies again", nil, maxRevives},
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
			c := &casualtyVMM{deadVMM: deadVMM{d.boxes[sb.ID].inst}, reviveErr: tc.reviveErr}
			c.revived = c // comes back still dead and still a casualty
			d.boxes[sb.ID].inst = c
			d.mu.Unlock()

			for range 2 * maxRevives {
				d.scanExits(ctx)
			}

			if _, ok := d.Get(sb.ID); ok {
				t.Fatal("a sandbox that would not stay up is still listed")
			}
			if n := fp.countEvents("restarted"); n != tc.restarted {
				t.Errorf("restarted events = %d, want %d", n, tc.restarted)
			}
			if n := fp.countEvents("exited"); n != 1 {
				t.Errorf("exited events = %d, want 1 (events %v)", n, fp.events)
			}
			if got := fp.history[sb.ID].ExitState; got != "exited" {
				t.Errorf("history exit state = %q, want exited", got)
			}
		})
	}
}

// Each death a new dockerd start caused is the daemon's doing, however close
// together: on the rig four restarts under 2 min apart destroyed every sandbox.
func TestScanNeverCountsDeathsEachUnderANewEngineStart(t *testing.T) {
	fp := newFakePersister(nil)
	d := New(Config{Runtime: runtimetest.NewFake(), Store: fp, IdleTimeout: time.Hour, HardTTL: time.Hour})
	ctx := context.Background()
	sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	d.mu.Lock()
	c := &casualtyVMM{deadVMM: deadVMM{d.boxes[sb.ID].inst}, engine: time.Unix(1_000_000, 0)}
	c.revived = c
	d.boxes[sb.ID].inst = c
	d.mu.Unlock()

	const restarts = 2*maxRevives + 1
	for range restarts {
		d.scanExits(ctx)
		c.engine = c.engine.Add(90 * time.Second)
	}
	if info, ok := d.Get(sb.ID); !ok || info.State != StateRunning {
		t.Fatalf("present %v, state %q: dockerd restarts destroyed a healthy sandbox", ok, info.State)
	}
	if n := fp.countEvents("restarted"); n != restarts {
		t.Errorf("restarted events = %d, want %d", n, restarts)
	}
}

// A revive that fails for any reason but a confirmed stop is dockerd still
// settling: it never counts and never destroys, is retried with backoff, and
// the stopped sandbox is not metered as running meanwhile. Counting it
// destroyed a healthy sandbox after ~40 s of containerd not being ready.
func TestScanKeepsRetryingACasualtyWhoseStartErrsWithoutAConfirmedStop(t *testing.T) {
	clk := &manualClock{t: time.Unix(1_000_000, 0)}
	fp := newFakePersister(nil)
	d := New(Config{Runtime: runtimetest.NewFake(), Store: fp, Clock: clk, IdleTimeout: time.Hour, HardTTL: 24 * time.Hour})
	ctx := context.Background()
	sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	d.mu.Lock()
	orig := d.boxes[sb.ID].inst
	c := &casualtyVMM{deadVMM: deadVMM{orig}, reviveErr: errors.New("Cannot connect to the Docker daemon")}
	d.boxes[sb.ID].inst = c
	d.mu.Unlock()

	const scans = 60 // eight minutes of scans 8 s apart
	for range scans {
		d.scanExits(ctx)
		clk.Advance(8 * time.Second)
	}
	if info, ok := d.Get(sb.ID); !ok || info.State != StateRunning {
		t.Fatalf("present %v, state %q: transient start errors destroyed the sandbox", ok, info.State)
	}
	if c.stopped || fp.countEvents("exited") != 0 {
		t.Fatalf("stopped %v, events %v; want nothing destroyed", c.stopped, fp.events)
	}
	if c.revives < maxRevives+2 || c.revives >= scans {
		t.Errorf("revive attempts = %d over %d scans, want retries that continue with backoff", c.revives, scans)
	}
	d.mu.Lock()
	metered := d.boxes[sb.ID].meter.running
	d.mu.Unlock()
	if metered {
		t.Error("a sandbox whose container is down is metered as running")
	}

	c.reviveErr, c.revived = nil, orig
	for range scans {
		d.scanExits(ctx)
		clk.Advance(8 * time.Second)
		if fp.countEvents("restarted") > 0 {
			break
		}
	}
	if fp.countEvents("restarted") != 1 {
		t.Fatalf("events = %v, want it revived once dockerd answers", fp.events)
	}
	d.mu.Lock()
	metered = d.boxes[sb.ID].meter.running
	d.mu.Unlock()
	if !metered {
		t.Error("a revived sandbox is not metered")
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

func TestReviveBackoffCapsAndNeverOverflows(t *testing.T) {
	for n, want := range map[int]time.Duration{1: 8 * time.Second, 2: 16 * time.Second, 5: 2 * time.Minute, 64: 2 * time.Minute, 1000: 2 * time.Minute} {
		if got := reviveBackoff(n); got != want {
			t.Errorf("reviveBackoff(%d) = %v, want %v", n, got, want)
		}
	}
}
