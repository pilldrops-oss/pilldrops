package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/runtimetest"
)

// Standby and pause are different tiers and must not be confused: a standby
// wakes by thawing the instance that is still there, a pause by restoring one
// that is gone.
func TestStandbyWakesByThawAndPauseByRestore(t *testing.T) {
	d, fake := newTestDaemon(t)
	ctx := context.Background()
	sb, err := d.Create(ctx, CreateSpec{Template: "t", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := d.Standby(ctx, sb.ID); err != nil {
		t.Fatalf("standby: %v", err)
	}
	if got, _ := d.Get(sb.ID); got.State != StateStandby {
		t.Fatalf("state = %s, want standby", got.State)
	}
	frozen, freezes, _ := fake.Frozen(sb.ID)
	if !frozen || freezes != 1 {
		t.Fatalf("instance frozen=%v freezes=%d, want frozen once", frozen, freezes)
	}
	if fake.Stopped(sb.ID) {
		t.Fatal("standby stopped the instance; it must stay resident")
	}
	bootsBefore := len(fake.ExecLog(sb.ID))

	if err := d.Resume(ctx, sb.ID); err != nil {
		t.Fatalf("resume from standby: %v", err)
	}
	frozen, _, thaws := fake.Frozen(sb.ID)
	if frozen || thaws != 1 {
		t.Fatalf("after resume frozen=%v thaws=%d, want thawed once", frozen, thaws)
	}
	// A restore would have booted a fresh instance and lost the exec history.
	if len(fake.ExecLog(sb.ID)) != bootsBefore {
		t.Fatal("waking from standby replaced the instance; it must be the same one, thawed")
	}

	// The disk tier still restores.
	if err := d.Pause(ctx, sb.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if got, _ := d.Get(sb.ID); got.State != StatePaused {
		t.Fatalf("state = %s, want paused", got.State)
	}
	if err := d.Resume(ctx, sb.ID); err != nil {
		t.Fatalf("resume from paused: %v", err)
	}
	if _, _, thaws := fake.Frozen(sb.ID); thaws != 0 {
		t.Fatalf("resuming from disk thawed the new instance %d time(s); it was never frozen", thaws)
	}
}

// Pause from standby thaws first, because the snapshot path pauses the VM itself.
func TestPauseFromStandbyThawsFirst(t *testing.T) {
	d, fake := newTestDaemon(t)
	ctx := context.Background()
	sb, _ := d.Create(ctx, CreateSpec{Template: "t", OrgID: "default"})
	if _, err := d.Standby(ctx, sb.ID); err != nil {
		t.Fatalf("standby: %v", err)
	}
	if err := d.Pause(ctx, sb.ID); err != nil {
		t.Fatalf("pause from standby: %v", err)
	}
	if _, _, thaws := fake.Frozen(sb.ID); thaws != 1 {
		t.Fatalf("thaws before the snapshot = %d, want 1", thaws)
	}
	if got, _ := d.Get(sb.ID); got.State != StatePaused {
		t.Fatalf("state = %s, want paused", got.State)
	}
}

// Exec on a standby sandbox wakes it rather than answering "paused".
func TestExecWakesAStandbySandbox(t *testing.T) {
	d, fake := newTestDaemon(t)
	ctx := context.Background()
	sb, _ := d.Create(ctx, CreateSpec{Template: "t", OrgID: "default"})
	if _, err := d.Standby(ctx, sb.ID); err != nil {
		t.Fatalf("standby: %v", err)
	}
	if _, err := d.Exec(ctx, sb.ID, rt.ExecSpec{Cmd: "echo hi"}); err != nil {
		t.Fatalf("exec on a standby sandbox: %v", err)
	}
	if frozen, _, thaws := fake.Frozen(sb.ID); frozen || thaws != 1 {
		t.Fatalf("frozen=%v thaws=%d after an exec, want thawed once", frozen, thaws)
	}
	if got, _ := d.Get(sb.ID); got.State != StateRunning {
		t.Fatalf("state = %s, want running", got.State)
	}
}

// A runtime whose instances cannot freeze answers ErrUnsupported, which the API
// renders 501 naming the runtime.
func TestStandbyUnsupportedInstance(t *testing.T) {
	d := New(Config{Runtime: noFreezeRuntime{runtimetest.NewFake()}, HardTTL: time.Hour, SnapshotDir: t.TempDir()})
	ctx := context.Background()
	sb, err := d.Create(ctx, CreateSpec{Template: "t", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := d.Standby(ctx, sb.ID); !errors.Is(err, rt.ErrUnsupported) {
		t.Fatalf("standby on a runtime that cannot freeze = %v, want ErrUnsupported", err)
	}
}

// The two timers: idle freezes, hibernate thaws and writes to disk, and a Touch
// pushes both out.
func TestReconcileIdleThenHibernate(t *testing.T) {
	clk := &manualClock{t: time.Unix(1000, 0)}
	fake := runtimetest.NewFake()
	d := New(Config{Runtime: fake, Clock: clk, SnapshotDir: t.TempDir(),
		IdleTimeout: 60 * time.Second, HibernateTimeout: 300 * time.Second, HardTTL: time.Hour})

	sb, _ := d.Create(context.Background(), CreateSpec{Template: "t"})

	clk.Advance(61 * time.Second)
	d.reconcileOnce(context.Background())
	if frozen, freezes, _ := fake.Frozen(sb.ID); !frozen || freezes != 1 {
		t.Fatalf("at 61 s frozen=%v freezes=%d, want frozen once", frozen, freezes)
	}
	if got, _ := d.Get(sb.ID); got.State != StateStandby {
		t.Fatalf("at 61 s state = %s, want standby", got.State)
	}

	clk.Advance(240 * time.Second) // 301 s total
	d.reconcileOnce(context.Background())
	if _, _, thaws := fake.Frozen(sb.ID); thaws != 1 {
		t.Fatalf("hibernate must thaw before it snapshots; thaws = %d", thaws)
	}
	if got, _ := d.Get(sb.ID); got.State != StatePaused {
		t.Fatalf("at 301 s state = %s, want paused", got.State)
	}
}

// Touch resets both timers.
func TestTouchPushesBothTimers(t *testing.T) {
	clk := &manualClock{t: time.Unix(1000, 0)}
	fake := runtimetest.NewFake()
	d := New(Config{Runtime: fake, Clock: clk, SnapshotDir: t.TempDir(),
		IdleTimeout: 60 * time.Second, HibernateTimeout: 300 * time.Second, HardTTL: time.Hour})
	sb, _ := d.Create(context.Background(), CreateSpec{Template: "t"})

	clk.Advance(50 * time.Second)
	d.Touch(sb.ID)
	clk.Advance(50 * time.Second) // 100 s in, but only 50 s since the touch
	d.reconcileOnce(context.Background())
	if got, _ := d.Get(sb.ID); got.State != StateRunning {
		t.Fatalf("state = %s, want running — Touch did not reset the idle timer", got.State)
	}
	clk.Advance(11 * time.Second)
	d.reconcileOnce(context.Background())
	if got, _ := d.Get(sb.ID); got.State != StateStandby {
		t.Fatalf("state = %s, want standby 61 s after the touch", got.State)
	}
}

// Standby bills RAM-seconds and not vCPU-seconds; a disk pause bills neither.
func TestMeterStandbyBillsRAMOnly(t *testing.T) {
	now := time.Unix(1000, 0)
	m := newMeter(2, 1024, 0, now) // 2 vCPU, 1 GiB

	m.standby(now)
	u := m.sample(now.Add(100 * time.Second))
	if u.RAMGBSeconds != 100 {
		t.Errorf("standby RAMGBSeconds = %v, want 100", u.RAMGBSeconds)
	}
	if u.VCPUSeconds != 0 || u.RunSeconds != 0 {
		t.Errorf("standby VCPUSeconds = %v, RunSeconds = %v, want 0 for both", u.VCPUSeconds, u.RunSeconds)
	}

	m2 := newMeter(2, 1024, 0, now)
	m2.freeze(now)
	p := m2.sample(now.Add(100 * time.Second))
	if p.RAMGBSeconds != 0 || p.VCPUSeconds != 0 {
		t.Errorf("paused usage = %+v, want no compute at all", p)
	}

	// Positive control: running bills both.
	m3 := newMeter(2, 1024, 0, now)
	r := m3.sample(now.Add(100 * time.Second))
	if r.RAMGBSeconds != 100 || r.VCPUSeconds != 200 {
		t.Errorf("running usage = %+v, want 100 RAM-GB-s and 200 vCPU-s", r)
	}
}

// noFreezeRuntime hands out instances that do not implement rt.Freezer.
type noFreezeRuntime struct{ inner rt.Runtime }

func (n noFreezeRuntime) Name() string          { return n.inner.Name() }
func (n noFreezeRuntime) Capabilities() rt.Caps { return rt.Caps{} }
func (n noFreezeRuntime) Prepare(ctx context.Context, t rt.Template) error {
	return n.inner.Prepare(ctx, t)
}
func (n noFreezeRuntime) Boot(ctx context.Context, s rt.BootSpec) (rt.Instance, error) {
	inst, err := n.inner.Boot(ctx, s)
	if err != nil {
		return nil, err
	}
	return plainInstance{inst}, nil
}
func (n noFreezeRuntime) Restore(ctx context.Context, snap rt.SnapshotRef, s rt.BootSpec) (rt.Instance, error) {
	return n.inner.Restore(ctx, snap, s)
}
func (n noFreezeRuntime) RecoverOrphans() []rt.Instance { return nil }

// plainInstance is the bare rt.Instance surface, with no optional capabilities.
type plainInstance struct{ inner rt.Instance }

func (p plainInstance) ID() string { return p.inner.ID() }
func (p plainInstance) Exec(ctx context.Context, s rt.ExecSpec) (rt.ExecResult, error) {
	return p.inner.Exec(ctx, s)
}
func (p plainInstance) FS() rt.FileSystem                     { return p.inner.FS() }
func (p plainInstance) Procs() ([]rt.Proc, error)             { return p.inner.Procs() }
func (p plainInstance) Signal(pid, sig int) error             { return p.inner.Signal(pid, sig) }
func (p plainInstance) ListenTarget(port int) (string, error) { return p.inner.ListenTarget(port) }
func (p plainInstance) TerminalTarget() (string, error)       { return p.inner.TerminalTarget() }
func (p plainInstance) StartServer(ctx context.Context, cmd string, env map[string]string) error {
	return p.inner.StartServer(ctx, cmd, env)
}
func (p plainInstance) Snapshot(ctx context.Context, dst rt.SnapshotRef) error {
	return p.inner.Snapshot(ctx, dst)
}
func (p plainInstance) Stop(ctx context.Context) error { return p.inner.Stop(ctx) }
func (p plainInstance) Metrics() rt.InstanceMetrics    { return p.inner.Metrics() }
