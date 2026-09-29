package orchestrator

import (
	"sync"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/config"
)

// The preview gate reads the store on every request (the sandbox's row, its
// public flag, the org's source networks), so a store outage refused every
// preview on every edge, and the source-network check let everyone through.
// These reads go through staleCache: fresh for previewFresh, and on a store
// error the last known answer, for up to previewStaleMax.
//
// previewFresh is PREVIEW_CACHE_MS, 0 by default: a visibility change or a
// destroy retracts links on the very next request. Under preview load an
// operator trades that for fewer store reads: at 2000, each replica reads a
// sandbox's row at most every 2 s, and a retracted link keeps working for up
// to that long.
var previewFresh = time.Duration(config.Int("PREVIEW_CACHE_MS", 0)) * time.Millisecond

const (
	previewStaleMax = 10 * time.Minute
	staleCacheMax   = 100_000
)

type staleCache struct {
	mu sync.Mutex
	m  map[string]staleEntry
}

type staleEntry struct {
	v  any
	at time.Time
}

// get returns key's value: the cached one when younger than fresh, else
// load's; when load fails, a cached one younger than previewStaleMax.
func (c *staleCache) get(key string, fresh time.Duration, now time.Time, load func() (any, error)) (any, error) {
	c.mu.Lock()
	e, ok := c.m[key]
	c.mu.Unlock()
	if ok && now.Sub(e.at) < fresh {
		return e.v, nil
	}
	v, err := load()
	if err != nil {
		if ok && now.Sub(e.at) < previewStaleMax {
			return e.v, nil
		}
		return nil, err
	}
	c.mu.Lock()
	if c.m == nil || len(c.m) >= staleCacheMax { // ponytail: wholesale reset, an LRU if it churns
		c.m = map[string]staleEntry{}
	}
	c.m[key] = staleEntry{v: v, at: now}
	c.mu.Unlock()
	return v, nil
}
