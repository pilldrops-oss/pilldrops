package daemon

import (
	"sync"
	"time"
)

// Durations is a minimal histogram: fixed buckets plus a sum and a count.
type Durations struct {
	mu      sync.Mutex
	buckets []float64
	counts  []int64
	sum     float64
	total   int64
}

// NewDurations builds a histogram over the given upper bounds, in seconds.
func NewDurations(upperBounds ...float64) *Durations {
	return &Durations{buckets: upperBounds, counts: make([]int64, len(upperBounds))}
}

// Observe records one duration.
func (d *Durations) Observe(v time.Duration) {
	secs := v.Seconds()
	d.mu.Lock()
	for i, ub := range d.buckets {
		if secs <= ub {
			d.counts[i]++
		}
	}
	d.sum += secs
	d.total++
	d.mu.Unlock()
}

// Snapshot returns copies of the cumulative bucket counts, the sum, and the total.
func (d *Durations) Snapshot() (bounds []float64, counts []int64, sum float64, total int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]float64(nil), d.buckets...), append([]int64(nil), d.counts...), d.sum, d.total
}

// Metrics is the daemon's observable state, read by the node's /metrics handler.
type Metrics struct {
	// ReconcileDuration is how long one reconcile tick took.
	ReconcileDuration *Durations
	// ReconcileOverruns counts ticks that hit their budget and were abandoned.
	ReconcileOverruns Counter
	// PauseDuration and ResumeDuration are the runtime's snapshot and restore costs.
	PauseDuration  *Durations
	ResumeDuration *Durations
	// ScanDuration is one OOM/crash scanner pass.
	ScanDuration *Durations
}

// Counter is a monotonic count safe for concurrent use.
type Counter struct {
	mu sync.Mutex
	n  int64
}

func (c *Counter) Inc()        { c.mu.Lock(); c.n++; c.mu.Unlock() }
func (c *Counter) Load() int64 { c.mu.Lock(); defer c.mu.Unlock(); return c.n }

func newDaemonMetrics() *Metrics {
	return &Metrics{
		ReconcileDuration: NewDurations(.001, .005, .01, .05, .1, .5, 1, 5, 15, 30),
		PauseDuration:     NewDurations(.1, .5, 1, 2.5, 5, 10, 30, 60, 120),
		ResumeDuration:    NewDurations(.1, .5, 1, 2.5, 5, 10, 30, 60, 120),
		ScanDuration:      NewDurations(.01, .05, .1, .5, 1, 2.5, 5, 7, 15),
	}
}

// Metrics returns the daemon's observable state.
func (d *Daemon) Metrics() *Metrics { return d.metrics }
