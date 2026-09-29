package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/runtime/runtimetest"
)

func TestUsageFlushIsRateLimitedButLosesNothing(t *testing.T) {
	clk := &manualClock{t: time.Unix(1000, 0)}
	fp := newFakePersister(nil)
	d := New(Config{
		Runtime: runtimetest.NewFake(), Clock: clk, Store: fp,
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	if _, err := d.Create(context.Background(), CreateSpec{Template: "t", VCPU: 2, MemMiB: 1024}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// 30 ticks inside one flush interval.
	for range 30 {
		clk.Advance(time.Second)
		d.reconcileOnce(context.Background())
	}
	fp.mu.Lock()
	earlyWrites, earlyVCPU := fp.usageWrites, fp.used.VCPUSeconds
	fp.mu.Unlock()
	if earlyWrites != 0 {
		t.Errorf("%d store writes inside one flush interval, want 0 — this is the per-second write load the change removes", earlyWrites)
	}
	if earlyVCPU != 0 {
		t.Errorf("vcpu-seconds = %v before the first flush, want 0", earlyVCPU)
	}

	clk.Advance(31 * time.Second)
	d.reconcileOnce(context.Background())

	fp.mu.Lock()
	writes, vcpu := fp.usageWrites, fp.used.VCPUSeconds
	fp.mu.Unlock()
	if writes != 1 {
		t.Errorf("store writes after crossing the interval = %d, want 1", writes)
	}
	// 61 elapsed seconds at 2 vCPU.
	if want := 61.0 * 2; vcpu != want {
		t.Errorf("vcpu-seconds = %v, want %v — deferring the flush must not change what is billed", vcpu, want)
	}
}

func TestPauseFlushesUsageImmediately(t *testing.T) {
	clk := &manualClock{t: time.Unix(1000, 0)}
	fp := newFakePersister(nil)
	d := New(Config{
		Runtime: runtimetest.NewFake(), Clock: clk, Store: fp,
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	sb, _ := d.Create(context.Background(), CreateSpec{Template: "t", VCPU: 1, MemMiB: 1024})

	clk.Advance(10 * time.Second) // well inside the flush interval
	if err := d.Pause(context.Background(), sb.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}
	fp.mu.Lock()
	vcpu := fp.used.VCPUSeconds
	fp.mu.Unlock()
	if vcpu != 10 {
		t.Errorf("vcpu-seconds after pause = %v, want 10 — pause must flush, not wait for the cadence", vcpu)
	}
}

func TestDestroyFlushesUsageImmediately(t *testing.T) {
	clk := &manualClock{t: time.Unix(1000, 0)}
	fp := newFakePersister(nil)
	d := New(Config{
		Runtime: runtimetest.NewFake(), Clock: clk, Store: fp,
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	sb, _ := d.Create(context.Background(), CreateSpec{Template: "t", VCPU: 1, MemMiB: 1024})

	clk.Advance(7 * time.Second)
	if err := d.Destroy(context.Background(), sb.ID); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	fp.mu.Lock()
	vcpu := fp.used.VCPUSeconds
	fp.mu.Unlock()
	if vcpu != 7 {
		t.Errorf("vcpu-seconds after destroy = %v, want 7", vcpu)
	}
}
