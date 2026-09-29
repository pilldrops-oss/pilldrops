package store

import (
	"testing"
	"time"
)

func TestLaunchP50IsTheMedian(t *testing.T) {
	st := openTemp(t)
	now := time.Now().Unix()
	for _, ms := range []int64{50_000, 10_000, 30_000, 20_000, 40_000} {
		if err := st.RecordLaunchSample(t.Context(), "u:alice", "python-3.11", ms, now); err != nil {
			t.Fatalf("RecordLaunchSample: %v", err)
		}
	}
	p50, n := st.LaunchP50(t.Context(), "python-3.11", 0)
	if n != 5 {
		t.Fatalf("n = %d, want 5", n)
	}
	if p50 != 30_000 {
		t.Fatalf("p50 = %d, want 30000", p50)
	}
}

func TestLaunchP50IsPerTemplate(t *testing.T) {
	st := openTemp(t)
	now := time.Now().Unix()
	_ = st.RecordLaunchSample(t.Context(), "u:alice", "python-3.11", 10_000, now)
	_ = st.RecordLaunchSample(t.Context(), "u:alice", "playwright", 90_000, now)

	if p50, n := st.LaunchP50(t.Context(), "python-3.11", 0); n != 1 || p50 != 10_000 {
		t.Fatalf("python p50=%d n=%d", p50, n)
	}
	if p50, n := st.LaunchP50(t.Context(), "playwright", 0); n != 1 || p50 != 90_000 {
		t.Fatalf("playwright p50=%d n=%d", p50, n)
	}
	// An unlaunched template reports n=0 rather than a zero that reads as instant.
	if p50, n := st.LaunchP50(t.Context(), "never-launched", 0); n != 0 || p50 != 0 {
		t.Fatalf("unknown template invented an eta: p50=%d n=%d", p50, n)
	}
}

func TestLaunchSampleIsClamped(t *testing.T) {
	st := openTemp(t)
	now := time.Now().Unix()
	_ = st.RecordLaunchSample(t.Context(), "u:evil", "python-3.11", 999_999_999, now)
	_ = st.RecordLaunchSample(t.Context(), "u:evil", "python-3.11", -5_000, now)

	p50, n := st.LaunchP50(t.Context(), "python-3.11", 0)
	if n != 2 {
		t.Fatalf("n = %d", n)
	}
	if p50 > maxLaunchSampleMs || p50 < minLaunchSampleMs {
		t.Fatalf("clamp did not hold: p50 = %d", p50)
	}
}

func TestLaunchP50IgnoresStaleSamples(t *testing.T) {
	st := openTemp(t)
	now := time.Now().Unix()
	old := now - 90*24*3600
	_ = st.RecordLaunchSample(t.Context(), "u:alice", "python-3.11", 120_000, old)
	_ = st.RecordLaunchSample(t.Context(), "u:alice", "python-3.11", 12_000, now)

	p50, n := st.LaunchP50(t.Context(), "python-3.11", now-30*24*3600)
	if n != 1 || p50 != 12_000 {
		t.Fatalf("stale sample counted: p50=%d n=%d", p50, n)
	}
}
