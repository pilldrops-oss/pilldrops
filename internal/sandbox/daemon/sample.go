package daemon

import (
	"context"
	"sync"
	"time"
)

// sampleCacheTTL is how long a live resource sample is reused: short enough to
// stay current, long enough that a 1s dashboard poll gets one real measurement.
const sampleCacheTTL = 3 * time.Second

// sampleGroup single-flights concurrent expensive samples and caches the result
// briefly. Zero value is ready to use.
type sampleGroup[T any] struct {
	mu      sync.Mutex
	entries map[string]*sampleEntry[T]
}

type sampleEntry[T any] struct {
	// done is closed when the in-flight measurement completes.
	done       chan struct{}
	val        T
	ok         bool
	freshUntil time.Time
}

// Do returns a cached sample when one is fresh, joins an in-flight measurement
// when one is running, and otherwise runs measure exactly once.
func (g *sampleGroup[T]) Do(ctx context.Context, key string, now time.Time, measure func(context.Context) (T, bool)) (T, bool) {
	g.mu.Lock()
	if g.entries == nil {
		g.entries = map[string]*sampleEntry[T]{}
	}
	if e, ok := g.entries[key]; ok {
		select {
		case <-e.done:
			if now.Before(e.freshUntil) {
				val, valid := e.val, e.ok
				g.mu.Unlock()
				return val, valid
			}
		default:
			// Still running; wait for it instead of starting another.
			done := e.done
			g.mu.Unlock()
			select {
			case <-done:
			case <-ctx.Done():
				var zero T
				return zero, false
			}
			g.mu.Lock()
			val, valid := e.val, e.ok
			g.mu.Unlock()
			return val, valid
		}
	}
	e := &sampleEntry[T]{done: make(chan struct{})}
	g.entries[key] = e
	g.mu.Unlock()

	val, ok := measure(ctx)

	g.mu.Lock()
	e.val, e.ok = val, ok
	// Cache a failure only briefly so a transient error does not pin a wrong answer.
	if ok {
		e.freshUntil = now.Add(sampleCacheTTL)
	} else {
		e.freshUntil = now.Add(sampleCacheTTL / 3)
	}
	close(e.done)
	g.mu.Unlock()
	return val, ok
}

// forget drops any cached sample for key.
func (g *sampleGroup[T]) forget(key string) {
	g.mu.Lock()
	delete(g.entries, key)
	g.mu.Unlock()
}
