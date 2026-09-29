package main

import "testing"

func TestPercentile(t *testing.T) {
	xs := []float64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	if p := percentile(xs, 50); p != 50 {
		t.Fatalf("p50 = %v, want 50", p)
	}
	if p := percentile(xs, 95); p < 90 || p > 100 {
		t.Fatalf("p95 = %v, want ~95", p)
	}
	if p := percentile(nil, 50); p != 0 {
		t.Fatalf("empty p50 = %v, want 0", p)
	}
}

// TestPercentileNearestRank pins the exact convention; the range check above
// would also accept int(p/100*(n-1)).
func TestPercentileNearestRank(t *testing.T) {
	t.Parallel()

	xs := []float64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	cases := []struct {
		name string
		p    float64
		want float64
	}{
		// Nearest-rank takes the lower median, not the average.
		{"p50 of n=10 is the 5th value", 50, 50},
		{"p95 of n=10 is the 10th value", 95, 100},
		{"p99 of n=10 is the 10th value", 99, 100},
		{"p100 is the max", 100, 100},
		{"p10 is the 1st value", 10, 10},
		{"p0 clamps to the min", 0, 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := percentile(xs, tc.p); got != tc.want {
				t.Fatalf("percentile(%v) = %v, want %v", tc.p, got, tc.want)
			}
		})
	}
}

func TestPercentileSmallN(t *testing.T) {
	t.Parallel()

	one := []float64{42}
	for _, p := range []float64{0, 50, 95, 99, 100} {
		if got := percentile(one, p); got != 42 {
			t.Fatalf("n=1 p%v = %v, want 42 — every percentile of one sample is that sample", p, got)
		}
	}

	two := []float64{10, 20}
	if got := percentile(two, 50); got != 10 {
		t.Fatalf("n=2 p50 = %v, want 10", got)
	}
	if got := percentile(two, 95); got != 20 {
		t.Fatalf("n=2 p95 = %v, want 20 — the tail of two samples is the slower one", got)
	}
}

// summarize keeps the samples in collection order, so percentile must not sort in place.
func TestPercentileDoesNotMutateInput(t *testing.T) {
	t.Parallel()

	xs := []float64{30, 10, 20}
	_ = percentile(xs, 95)
	if xs[0] != 30 || xs[1] != 10 || xs[2] != 20 {
		t.Fatalf("percentile mutated its input: %v", xs)
	}
}

func TestSummarizeExcludesFailures(t *testing.T) {
	t.Parallel()

	samples := []Sample{
		{Index: 0, ReadyMS: 1000, OK: true},
		{Index: 1, ReadyMS: 0, OK: false, Err: "never became usable"},
		{Index: 2, ReadyMS: 2000, OK: true},
	}
	got := summarize(samples, func(s Sample) float64 { return s.ReadyMS })
	if got.N != 2 {
		t.Fatalf("N = %d, want 2 (the failed sample must not be counted)", got.N)
	}
	if got.Min != 1000 {
		t.Fatalf("Min = %v, want 1000 — a failure's zero must not become the minimum", got.Min)
	}
	if got.Mean != 1500 {
		t.Fatalf("Mean = %v, want 1500", got.Mean)
	}
}

// A run where nothing worked reports N=0 so a reader can tell "no data" from "fast".
func TestSummarizeAllFailed(t *testing.T) {
	t.Parallel()

	got := summarize([]Sample{{OK: false}, {OK: false}}, func(s Sample) float64 { return s.ReadyMS })
	if got.N != 0 {
		t.Fatalf("N = %d, want 0", got.N)
	}
}

func TestTallyErrors(t *testing.T) {
	tl := tally{}
	tl.add(200)
	tl.add(200)
	tl.add(503)
	if tl.ok != 2 || tl.byStatus[503] != 1 {
		t.Fatalf("tally = %+v", tl)
	}
}
