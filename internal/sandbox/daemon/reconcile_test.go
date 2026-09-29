package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/runtime/runtimetest"
)

// The idle timer now freezes the sandbox in RAM rather than writing it to disk;
// the disk tier is the hibernate timer below.
func TestReconcileIdleAutoStandby(t *testing.T) {
	clk := &manualClock{t: time.Unix(1000, 0)}
	fake := runtimetest.NewFake()
	d := New(Config{Runtime: fake, Clock: clk, IdleTimeout: 30 * time.Second, HardTTL: time.Hour})

	sb, _ := d.Create(context.Background(), CreateSpec{Template: "t"})

	clk.Advance(20 * time.Second)
	d.reconcileOnce(context.Background())
	if got, _ := d.Get(sb.ID); got.State != StateRunning {
		t.Fatalf("state = %s, want running", got.State)
	}

	clk.Advance(20 * time.Second)
	d.reconcileOnce(context.Background())
	if got, _ := d.Get(sb.ID); got.State != StateStandby {
		t.Fatalf("state = %s, want standby", got.State)
	}
}

func TestReconcileNeverIdlePause(t *testing.T) {
	clk := &manualClock{t: time.Unix(1000, 0)}
	fake := runtimetest.NewFake()
	d := New(Config{Runtime: fake, Clock: clk, IdleTimeout: 30 * time.Second, HardTTL: time.Hour})

	sb, _ := d.Create(context.Background(), CreateSpec{Template: "t", IdleTimeout: -1})

	clk.Advance(10 * time.Minute) // way past the 30s default
	d.reconcileOnce(context.Background())
	if got, _ := d.Get(sb.ID); got.State != StateRunning {
		t.Fatalf("always-on sandbox state = %s, want running (must not idle-standby)", got.State)
	}
}

func TestReconcilePerSandboxIdleOverride(t *testing.T) {
	clk := &manualClock{t: time.Unix(1000, 0)}
	fake := runtimetest.NewFake()
	d := New(Config{Runtime: fake, Clock: clk, IdleTimeout: 30 * time.Second, HardTTL: time.Hour})

	sb, _ := d.Create(context.Background(), CreateSpec{Template: "t", IdleTimeout: 5 * time.Minute})

	clk.Advance(time.Minute) // past the 30s default, within the 5m override
	d.reconcileOnce(context.Background())
	if got, _ := d.Get(sb.ID); got.State != StateRunning {
		t.Fatalf("state = %s, want running (override not honored)", got.State)
	}
	clk.Advance(5 * time.Minute) // past the override
	d.reconcileOnce(context.Background())
	if got, _ := d.Get(sb.ID); got.State != StateStandby {
		t.Fatalf("state = %s, want standby at the per-sandbox threshold", got.State)
	}
}

// SetIdleTimeout on a running sandbox moves the threshold: the old value would
// not pause at 31 s, the new one does; and the new value is visible on Get.
func TestReconcileIdleTimeoutChangedLive(t *testing.T) {
	clk := &manualClock{t: time.Unix(1000, 0)}
	fake := runtimetest.NewFake()
	d := New(Config{Runtime: fake, Clock: clk, IdleTimeout: 5 * time.Minute, HardTTL: time.Hour})

	sb, _ := d.Create(context.Background(), CreateSpec{Template: "t"})
	if err := d.SetIdleTimeout(context.Background(), sb.ID, 30*time.Second); err != nil {
		t.Fatalf("set idle: %v", err)
	}
	if got, _ := d.Get(sb.ID); got.IdleTimeout != 30*time.Second || got.State != StateRunning {
		t.Fatalf("after set: idle=%s state=%s, want 30s running", got.IdleTimeout, got.State)
	}
	clk.Advance(31 * time.Second)
	d.reconcileOnce(context.Background())
	if got, _ := d.Get(sb.ID); got.State != StateStandby {
		t.Fatalf("state = %s, want standby 31 s after a live idle_timeout of 30 s", got.State)
	}

	// Control: the same clock advance under the old 5 m value does not pause.
	sb2, _ := d.Create(context.Background(), CreateSpec{Template: "t"})
	clk.Advance(31 * time.Second)
	d.reconcileOnce(context.Background())
	if got, _ := d.Get(sb2.ID); got.State != StateRunning {
		t.Fatalf("control state = %s, want running under the 5 m default", got.State)
	}
	if err := d.SetIdleTimeout(context.Background(), "nope", time.Minute); err == nil {
		t.Fatal("SetIdleTimeout on an unknown id returned nil")
	}
}

func TestReconcileHardTTLStop(t *testing.T) {
	clk := &manualClock{t: time.Unix(1000, 0)}
	fake := runtimetest.NewFake()
	d := New(Config{Runtime: fake, Clock: clk, IdleTimeout: time.Hour, HardTTL: time.Minute})

	sb, _ := d.Create(context.Background(), CreateSpec{Template: "t"})

	clk.Advance(2 * time.Minute)
	d.reconcileOnce(context.Background())
	if _, ok := d.Get(sb.ID); ok {
		t.Fatalf("sandbox should be stopped and removed after hard TTL")
	}
}

// A negative Timeout means no hard TTL: the sandbox idle-pauses instead of being destroyed.
func TestReconcilePersistentSurvivesHardTTL(t *testing.T) {
	clk := &manualClock{t: time.Unix(1000, 0)}
	fake := runtimetest.NewFake()
	d := New(Config{Runtime: fake, Clock: clk,
		IdleTimeout: 30 * time.Second, HibernateTimeout: time.Minute, HardTTL: time.Minute})

	sb, err := d.Create(context.Background(), CreateSpec{Template: "t", Timeout: -1})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got, _ := d.Get(sb.ID); !got.ExpiresAt.IsZero() {
		t.Fatalf("persistent sandbox ExpiresAt = %v, want zero (no expiry)", got.ExpiresAt)
	}

	clk.Advance(10000 * time.Minute)
	d.reconcileOnce(context.Background())

	got, ok := d.Get(sb.ID)
	if !ok {
		t.Fatal("persistent sandbox was destroyed by the TTL reaper")
	}
	if got.State != StatePaused {
		t.Fatalf("state = %s, want paused (hibernated, not destroyed)", got.State)
	}
	if err := d.Resume(context.Background(), sb.ID); err != nil {
		t.Fatalf("resume after long pause: %v", err)
	}
	if got, _ := d.Get(sb.ID); got.State != StateRunning {
		t.Fatalf("state after resume = %s, want running", got.State)
	}
}

// LastActiveAt stops advancing while paused, so AbandonAfter measures time since last resume.
func TestReconcilePersistentAbandoned(t *testing.T) {
	clk := &manualClock{t: time.Unix(1000, 0)}
	fake := runtimetest.NewFake()
	d := New(Config{
		Runtime: fake, Clock: clk,
		IdleTimeout: 30 * time.Second, HardTTL: time.Minute,
		AbandonAfter: 72 * time.Hour,
	})

	sb, _ := d.Create(context.Background(), CreateSpec{Template: "t", Timeout: -1})

	clk.Advance(48 * time.Hour)
	d.reconcileOnce(context.Background())
	if _, ok := d.Get(sb.ID); !ok {
		t.Fatal("sandbox abandoned before AbandonAfter elapsed")
	}

	clk.Advance(48 * time.Hour)
	d.reconcileOnce(context.Background())
	if _, ok := d.Get(sb.ID); ok {
		t.Fatal("abandoned persistent sandbox should have been destroyed")
	}
}

func TestReconcilePersistentResumeResetsAbandonClock(t *testing.T) {
	clk := &manualClock{t: time.Unix(1000, 0)}
	fake := runtimetest.NewFake()
	d := New(Config{
		Runtime: fake, Clock: clk,
		IdleTimeout: 30 * time.Second, HardTTL: time.Minute,
		AbandonAfter: 72 * time.Hour,
	})

	sb, _ := d.Create(context.Background(), CreateSpec{Template: "t", Timeout: -1})

	for range 5 { // five cycles of "away two days, come back"
		clk.Advance(48 * time.Hour)
		d.reconcileOnce(context.Background())
		if _, ok := d.Get(sb.ID); !ok {
			t.Fatal("actively used workstation was abandoned")
		}
		if err := d.Resume(context.Background(), sb.ID); err != nil {
			t.Fatalf("resume: %v", err)
		}
	}
}

func TestReconcileAbandonDisabled(t *testing.T) {
	clk := &manualClock{t: time.Unix(1000, 0)}
	fake := runtimetest.NewFake()
	d := New(Config{
		Runtime: fake, Clock: clk,
		IdleTimeout: 30 * time.Second, HardTTL: time.Minute,
		AbandonAfter: -1,
	})

	sb, _ := d.Create(context.Background(), CreateSpec{Template: "t", Timeout: -1})

	clk.Advance(365 * 24 * time.Hour)
	d.reconcileOnce(context.Background())
	if _, ok := d.Get(sb.ID); !ok {
		t.Fatal("AbandonAfter<0 must disable abandonment")
	}
}

// ExpiresAt round-trips through the store as 0; restore must read that as no expiry, not 1970.
func TestPersistentSurvivesDaemonRestart(t *testing.T) {
	clk := &manualClock{t: time.Unix(1_000_000, 0)}
	fake := runtimetest.NewFake()
	fp := newFakePersister(nil)
	d := New(Config{
		Runtime: fake, Store: fp, Clock: clk,
		SnapshotDir: t.TempDir(),
		IdleTimeout: 30 * time.Second, HardTTL: time.Minute,
	})

	sb, _ := d.Create(context.Background(), CreateSpec{Template: "t", Timeout: -1})
	if err := d.Pause(context.Background(), sb.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}

	fp.mu.Lock()
	rec, ok := fp.saved[sb.ID]
	fp.mu.Unlock()
	if !ok {
		t.Fatal("no persisted record")
	}
	if rec.ExpiresAt != 0 {
		t.Fatalf("persisted ExpiresAt = %d, want 0 (never expires)", rec.ExpiresAt)
	}

	d2 := New(Config{
		Runtime: fake, Store: newFakePersister([]PersistedSandbox{rec}), Clock: clk,
		SnapshotDir: t.TempDir(),
		IdleTimeout: 30 * time.Second, HardTTL: time.Minute,
	})
	d2.Start()
	defer d2.Stop()

	got, ok := d2.Get(sb.ID)
	if !ok {
		t.Fatal("persistent sandbox was dropped on daemon restart")
	}
	if !got.ExpiresAt.IsZero() {
		t.Fatalf("ExpiresAt after restart = %v, want zero", got.ExpiresAt)
	}
	clk.Advance(100 * time.Minute)
	d2.reconcileOnce(context.Background())
	if _, ok := d2.Get(sb.ID); !ok {
		t.Fatal("persistent sandbox reaped after restart")
	}
}

func TestRefreshDoesNotDemotePersistent(t *testing.T) {
	clk := &manualClock{t: time.Unix(1000, 0)}
	fake := runtimetest.NewFake()
	d := New(Config{Runtime: fake, Clock: clk, IdleTimeout: time.Hour, HardTTL: time.Minute})

	sb, _ := d.Create(context.Background(), CreateSpec{Template: "t", Timeout: -1})
	if err := d.Refresh(t.Context(), sb.ID); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got, _ := d.Get(sb.ID); !got.ExpiresAt.IsZero() {
		t.Fatalf("ExpiresAt after refresh = %v, want zero (still persistent)", got.ExpiresAt)
	}
}

func TestSetTimeoutPromotesToPersistent(t *testing.T) {
	clk := &manualClock{t: time.Unix(1000, 0)}
	fake := runtimetest.NewFake()
	d := New(Config{Runtime: fake, Clock: clk, IdleTimeout: time.Hour, HardTTL: time.Minute})

	sb, _ := d.Create(context.Background(), CreateSpec{Template: "t"})
	if got, _ := d.Get(sb.ID); got.ExpiresAt.IsZero() {
		t.Fatal("a default sandbox must start TTL-bound")
	}
	if err := d.SetTimeout(t.Context(), sb.ID, -1); err != nil {
		t.Fatalf("set timeout: %v", err)
	}

	clk.Advance(100 * time.Minute)
	d.reconcileOnce(context.Background())
	if _, ok := d.Get(sb.ID); !ok {
		t.Fatal("promoted sandbox was reaped by the TTL reaper")
	}
}

func TestReconcileBillsStorageWhilePaused(t *testing.T) {
	clk := &manualClock{t: time.Unix(1000, 0)}
	fake := runtimetest.NewFake()
	fp := newFakePersister(nil)
	d := New(Config{
		Runtime: fake, Store: fp, Clock: clk,
		SnapshotDir: t.TempDir(),
		IdleTimeout: 30 * time.Second, HardTTL: time.Hour,
	})

	sb, _ := d.Create(context.Background(), CreateSpec{
		Template: "t", Timeout: -1, VCPU: 1, MemMiB: 1024, StorageMB: 10240,
	})
	if err := d.Pause(context.Background(), sb.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}
	atPause := fp.billed()

	clk.Advance(24 * time.Hour)
	d.reconcileOnce(context.Background())

	after := fp.billed()
	if after.RAMGBSeconds != atPause.RAMGBSeconds {
		t.Errorf("RAM billed while paused: %.1f → %.1f (compute must stop)",
			atPause.RAMGBSeconds, after.RAMGBSeconds)
	}
	if after.RunSeconds != atPause.RunSeconds {
		t.Errorf("active hours accrued while paused: %.1f → %.1f — a parked workstation must consume no entitlement",
			atPause.RunSeconds, after.RunSeconds)
	}
	// 10 GB * 86400s.
	if want := atPause.StorageGBSeconds + 864000; after.StorageGBSeconds < want-1 || after.StorageGBSeconds > want+1 {
		t.Errorf("storage billed = %.0f GB-s, want ~%.0f (10 GB for 24h while paused)",
			after.StorageGBSeconds, want)
	}
}

// Two boxes of different sizes running the same wall time must contribute the same hours.
func TestRunSecondsIsWallClockNotVCPUScaled(t *testing.T) {
	clk := &manualClock{t: time.Unix(1000, 0)}
	fake := runtimetest.NewFake()
	fp := newFakePersister(nil)
	d := New(Config{
		Runtime: fake, Store: fp, Clock: clk,
		SnapshotDir: t.TempDir(),
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})

	_, _ = d.Create(context.Background(), CreateSpec{Template: "t", VCPU: 1, MemMiB: 1024})
	_, _ = d.Create(context.Background(), CreateSpec{Template: "t", VCPU: 4, MemMiB: 1024})

	clk.Advance(time.Hour)
	d.reconcileOnce(context.Background())

	u := fp.billed()
	if want := 2 * 3600.0; u.RunSeconds < want-1 || u.RunSeconds > want+1 {
		t.Errorf("run seconds = %.0f, want %.0f (2 boxes x 1h, regardless of size)", u.RunSeconds, want)
	}
	// vCPU-seconds is size-weighted, so it cannot stand in for active hours.
	if want := 5 * 3600.0; u.VCPUSeconds < want-1 || u.VCPUSeconds > want+1 {
		t.Errorf("vcpu seconds = %.0f, want %.0f", u.VCPUSeconds, want)
	}
}
