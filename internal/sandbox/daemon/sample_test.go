package daemon

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSampleGroupDeduplicatesConcurrentCallers(t *testing.T) {
	var g sampleGroup[int]
	var calls atomic.Int32
	release := make(chan struct{})
	now := time.Unix(1000, 0)

	const callers = 20
	var wg sync.WaitGroup
	results := make([]int, callers)
	wg.Add(callers)
	for i := range callers {
		go func() {
			defer wg.Done()
			v, _ := g.Do(context.Background(), "sb-1", now, func(context.Context) (int, bool) {
				calls.Add(1)
				<-release // hold every caller inside the measurement
				return 42, true
			})
			results[i] = v
		}()
	}
	// Let the goroutines pile up on the in-flight measurement.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("measured %d times for %d concurrent callers, want 1", got, callers)
	}
	for i, v := range results {
		if v != 42 {
			t.Errorf("caller %d got %d, want every caller to receive the shared result", i, v)
		}
	}
}

func TestSampleGroupServesFromCache(t *testing.T) {
	var g sampleGroup[int]
	var calls atomic.Int32
	measure := func(context.Context) (int, bool) { calls.Add(1); return 7, true }

	now := time.Unix(1000, 0)
	for range 5 {
		if v, ok := g.Do(context.Background(), "sb-1", now, measure); !ok || v != 7 {
			t.Fatalf("Do = %v,%v", v, ok)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("measured %d times inside the TTL, want 1", got)
	}

	g.Do(context.Background(), "sb-1", now.Add(sampleCacheTTL+time.Second), measure)
	if got := calls.Load(); got != 2 {
		t.Errorf("measured %d times after the TTL expired, want 2", got)
	}
}

func TestSampleGroupExpiresFailuresSooner(t *testing.T) {
	var g sampleGroup[int]
	var calls atomic.Int32
	now := time.Unix(1000, 0)
	fail := func(context.Context) (int, bool) { calls.Add(1); return 0, false }

	g.Do(context.Background(), "sb-1", now, fail)
	g.Do(context.Background(), "sb-1", now.Add(sampleCacheTTL/6), fail)
	if got := calls.Load(); got != 1 {
		t.Errorf("calls = %d inside the failure window, want 1", got)
	}
	// Past the failure window but inside the success TTL.
	g.Do(context.Background(), "sb-1", now.Add(sampleCacheTTL/2), fail)
	if got := calls.Load(); got != 2 {
		t.Errorf("calls = %d after the failure window, want 2", got)
	}
}

func TestSampleGroupIsPerKey(t *testing.T) {
	var g sampleGroup[string]
	now := time.Unix(1000, 0)
	a, _ := g.Do(context.Background(), "sb-a", now, func(context.Context) (string, bool) { return "a", true })
	b, _ := g.Do(context.Background(), "sb-b", now, func(context.Context) (string, bool) { return "b", true })
	if a != "a" || b != "b" {
		t.Errorf("got %q/%q, want a/b — samples must not leak across sandboxes", a, b)
	}
}

func TestSampleGroupForget(t *testing.T) {
	var g sampleGroup[int]
	var calls atomic.Int32
	now := time.Unix(1000, 0)
	measure := func(context.Context) (int, bool) { calls.Add(1); return 1, true }

	g.Do(context.Background(), "sb-1", now, measure)
	g.forget("sb-1")
	g.Do(context.Background(), "sb-1", now, measure)
	if got := calls.Load(); got != 2 {
		t.Errorf("calls = %d, want 2 — forget must drop the cached entry", got)
	}
}
