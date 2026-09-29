package cron

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// blockingRunner holds every job inside RunInSandbox until release is closed,
// recording the peak number in flight at once.
type blockingRunner struct {
	mu      sync.Mutex
	inFler  int
	peak    int
	entered chan struct{}
	release chan struct{}
}

func newBlockingRunner(n int) *blockingRunner {
	return &blockingRunner{entered: make(chan struct{}, n), release: make(chan struct{})}
}

func (b *blockingRunner) RunInSandbox(ctx context.Context, orgID, sandboxID, cmd string) (string, int, error) {
	b.mu.Lock()
	b.inFler++
	if b.inFler > b.peak {
		b.peak = b.inFler
	}
	b.mu.Unlock()

	b.entered <- struct{}{}
	<-b.release

	b.mu.Lock()
	b.inFler--
	b.mu.Unlock()
	return "", 0, nil
}

func (b *blockingRunner) peakInFlight() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.peak
}

// Three jobs are parked inside the runner at once; a serial sweep would deadlock on the first.
func TestSlowJobDoesNotBlockTheJobsBehindIt(t *testing.T) {
	const n = 3
	due := make([]store.CronJob, 0, n)
	for _, id := range []string{"cron_a", "cron_b", "cron_c"} {
		due = append(due, job(id, "* * * * *", "sleep", 100))
	}
	st := &fakeStore{due: due}
	rn := newBlockingRunner(n)
	s := NewSweeper(st, rn, nil)

	swept := make(chan struct{})
	go func() { s.SweepOnce(context.Background(), time.Unix(200, 0)); close(swept) }()

	for i := 0; i < n; i++ {
		select {
		case <-rn.entered:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d jobs started — the sweep is still serial", i, n)
		}
	}
	close(rn.release)

	select {
	case <-swept:
	case <-time.After(5 * time.Second):
		t.Fatal("SweepOnce did not return after its jobs finished")
	}

	if got := rn.peakInFlight(); got != n {
		t.Errorf("peak jobs in flight = %d, want %d", got, n)
	}
	if len(st.runs()) != n {
		t.Errorf("recorded %d outcomes, want %d", len(st.runs()), n)
	}
}

func TestSweepConcurrencyIsBounded(t *testing.T) {
	total := sweepConcurrency + 4
	due := make([]store.CronJob, 0, total)
	for i := 0; i < total; i++ {
		due = append(due, job("cron_"+string(rune('a'+i)), "* * * * *", "sleep", 100))
	}
	st := &fakeStore{due: due}
	rn := newBlockingRunner(total)
	s := NewSweeper(st, rn, nil)

	swept := make(chan struct{})
	go func() { s.SweepOnce(context.Background(), time.Unix(200, 0)); close(swept) }()

	// Exactly sweepConcurrency jobs may be parked at once; the rest wait on the semaphore.
	for i := 0; i < sweepConcurrency; i++ {
		select {
		case <-rn.entered:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d jobs started, want the full pool of %d", i, sweepConcurrency)
		}
	}
	select {
	case <-rn.entered:
		t.Fatalf("more than %d jobs ran at once — the pool is not bounding the fan-out", sweepConcurrency)
	case <-time.After(100 * time.Millisecond):
	}

	close(rn.release)
	select {
	case <-swept:
	case <-time.After(10 * time.Second):
		t.Fatal("SweepOnce did not drain")
	}
	if got := rn.peakInFlight(); got > sweepConcurrency {
		t.Errorf("peak jobs in flight = %d, want at most %d", got, sweepConcurrency)
	}
	if len(st.runs()) != total {
		t.Errorf("recorded %d outcomes, want all %d jobs to have run", len(st.runs()), total)
	}
}
