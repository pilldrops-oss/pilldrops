package orchestrator

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// doWithRetry is the unconditional spelling: production always passes a
// predicate, so this lives here rather than beside doWithRetryIf.
func doWithRetry(ctx context.Context, do func() (*http.Response, error), attempts int, backoff func(int) time.Duration) (*http.Response, error) {
	return doWithRetryIf(ctx, do, attempts, backoff, func(error) bool { return true })
}

func TestDoWithRetryRetriesTransportErrors(t *testing.T) {
	calls := 0
	do := func() (*http.Response, error) {
		calls++
		if calls < 3 {
			return nil, errors.New("connection refused")
		}
		return &http.Response{StatusCode: 200}, nil
	}
	resp, err := doWithRetry(t.Context(), do, 2, func(int) time.Duration { return 0 })
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("resp=%v err=%v, want a 200", resp, err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3 (1 + 2 retries)", calls)
	}
}

func TestDoWithRetryGivesUp(t *testing.T) {
	calls := 0
	do := func() (*http.Response, error) { calls++; return nil, errors.New("boom") }
	if _, err := doWithRetry(t.Context(), do, 2, func(int) time.Duration { return 0 }); err == nil {
		t.Error("want error after exhausting retries")
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

// A received response, even a 5xx, is never retried: the node may have executed a non-idempotent op.
func TestDoWithRetryDoesNotRetryResponses(t *testing.T) {
	calls := 0
	do := func() (*http.Response, error) { calls++; return &http.Response{StatusCode: 500}, nil }
	resp, err := doWithRetry(t.Context(), do, 2, func(int) time.Duration { return 0 })
	if err != nil || resp.StatusCode != 500 {
		t.Fatalf("resp=%v err=%v, want the 500 passed through", resp, err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (a received response is never retried)", calls)
	}
}

// A cancelled caller must not sit out its backoff.
func TestRetryStopsWhenTheCallerGoesAway(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	do := func() (*http.Response, error) {
		calls++
		cancel() // the client disconnects during the first attempt
		return nil, errors.New("dial failed")
	}

	start := time.Now()
	_, err := doWithRetry(ctx, do, 3, func(int) time.Duration { return 2 * time.Second })
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Errorf("took %v — a cancelled request must not sit out its backoff", elapsed)
	}
	if calls != 1 {
		t.Errorf("made %d attempts after cancellation, want 1", calls)
	}
	// The transport error explains the retry; context.Canceled would lose that.
	if err == nil || !strings.Contains(err.Error(), "dial failed") {
		t.Errorf("err = %v, want the transport error that prompted the retry", err)
	}
}

// Without jitter every request failed by a node restart retries at the same instant.
func TestRetryBackoffIsJittered(t *testing.T) {
	seen := map[time.Duration]int{}
	const draws = 200
	for range draws {
		seen[retryBackoff(1)]++
	}
	if len(seen) < draws/10 {
		t.Errorf("retryBackoff(1) produced only %d distinct delays in %d draws — "+
			"a fixed schedule reconverges every retry onto the same instant", len(seen), draws)
	}

	// Equal jitter, not full: a draw near zero spends the retry budget before the node is back.
	for i := 1; i <= 3; i++ {
		base := time.Duration(i) * 100 * time.Millisecond
		for range 100 {
			d := retryBackoff(i)
			if d < base/2 || d >= base {
				t.Fatalf("retryBackoff(%d) = %v, want it within [%v, %v)", i, d, base/2, base)
			}
		}
	}
}

func TestRetryBackoffGrows(t *testing.T) {
	// Compare floors, which are deterministic, rather than individual draws.
	if retryBackoff(2) < 100*time.Millisecond {
		t.Errorf("retryBackoff(2) = %v, want at least the 100ms floor", retryBackoff(2))
	}
	if retryBackoff(1) >= 100*time.Millisecond {
		t.Errorf("retryBackoff(1) = %v, want under 100ms", retryBackoff(1))
	}
}

func TestSleepCtx(t *testing.T) {
	if !sleepCtx(context.Background(), time.Millisecond) {
		t.Error("a completed sleep must report true")
	}
	// A zero duration is not a reason to sleep, but a cancelled context is still a reason to stop.
	if !sleepCtx(context.Background(), 0) {
		t.Error("a zero-duration sleep on a live context must report true")
	}
	done, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepCtx(done, 0) {
		t.Error("a zero-duration sleep on a cancelled context must report false")
	}
	if sleepCtx(done, time.Hour) {
		t.Error("a cancelled context must cut the sleep short")
	}
}
