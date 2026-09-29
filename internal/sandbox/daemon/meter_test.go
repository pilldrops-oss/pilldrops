package daemon

import (
	"testing"
	"time"
)

func TestMeterAccrual(t *testing.T) {
	t0 := time.Unix(1000, 0)
	m := newMeter(2 /*vcpu*/, 1024 /*MiB*/, 2048 /*storage MB*/, t0)

	u := m.sample(t0.Add(10 * time.Second))
	if u.VCPUSeconds != 20 || u.RAMGBSeconds != 10 {
		t.Fatalf("sample = %.1f vcpu, %.1f ram; want 20, 10", u.VCPUSeconds, u.RAMGBSeconds)
	}
	if u.StorageGBSeconds != 20 {
		t.Fatalf("storage = %.1f; want 20", u.StorageGBSeconds)
	}

	// Paused: compute stops, storage keeps accruing.
	m.freeze(t0.Add(10 * time.Second))
	u = m.sample(t0.Add(110 * time.Second))
	if u.VCPUSeconds != 0 || u.RAMGBSeconds != 0 {
		t.Fatalf("paused sample = %.1f, %.1f; want 0, 0", u.VCPUSeconds, u.RAMGBSeconds)
	}
	if u.StorageGBSeconds != 200 { // 2 GB * 100s
		t.Fatalf("paused storage = %.1f; want 200 (storage accrues while paused)", u.StorageGBSeconds)
	}

	m.resume(t0.Add(110 * time.Second))
	u = m.sample(t0.Add(115 * time.Second))
	if u.VCPUSeconds != 10 || u.RAMGBSeconds != 5 {
		t.Fatalf("resumed sample = %.1f, %.1f; want 10, 5", u.VCPUSeconds, u.RAMGBSeconds)
	}
	if u.StorageGBSeconds != 10 {
		t.Fatalf("resumed storage = %.1f; want 10", u.StorageGBSeconds)
	}

	tot := m.total()
	if tot.StorageGBSeconds != 230 { // 2 GB * 115s
		t.Fatalf("total storage = %.1f; want 230", tot.StorageGBSeconds)
	}
}

func TestMeterPeekStorageWhilePaused(t *testing.T) {
	t0 := time.Unix(1000, 0)
	m := newMeter(1, 1024, 1024 /*1 GB*/, t0)
	m.freeze(t0)

	pk := m.peek(t0.Add(60 * time.Second))
	if pk.StorageGBSeconds != 60 {
		t.Fatalf("peek storage = %.1f; want 60", pk.StorageGBSeconds)
	}
	s2 := m.sample(t0.Add(60 * time.Second))
	if s2.StorageGBSeconds != 60 {
		t.Fatalf("sample after peek = %.1f; want 60 (peek must not consume)", s2.StorageGBSeconds)
	}
}
