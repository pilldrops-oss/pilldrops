package httpapi

import (
	"sync"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/auth/orgassert"
	"github.com/pilldrops-oss/pilldrops/internal/nodetls"
)

// requireSecureHop reads the fleet-wide enforcement switch; a function so tests
// can flip it without rebuilding a Server.
func requireSecureHop() bool { return nodetls.RequireSecureHop() }

// Org assertions are single-use: the node-id binding stops replay at other
// nodes, this cache stops a second use here. Retries mint a fresh assertion.

// nonceTTL covers the assertion's validity window plus clock skew both ways:
// a node behind by MaxSkew first sees a nonce MaxSkew before it was minted and
// must still hold it when a node ahead would stop accepting it.
const nonceTTL = orgassert.TTL + 2*orgassert.MaxSkew + 30*time.Second

// nonceCache remembers assertion nonces for nonceTTL. Bounded by time, not
// size: a size cap would start accepting replays exactly when the node is busiest.
type nonceCache struct {
	mu      sync.Mutex
	seen    map[string]time.Time
	sweepAt time.Time
	now     func() time.Time
}

func newNonceCache() *nonceCache {
	return &nonceCache{seen: map[string]time.Time{}, now: time.Now}
}

// use records nonce and reports whether this is its first use. Claim and check
// happen under one lock so two concurrent replays cannot both pass.
func (c *nonceCache) use(nonce string) bool {
	if nonce == "" {
		return true // the caller decides whether a missing nonce is allowed
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = map[string]time.Time{}
	}
	if now.After(c.sweepAt) {
		for k, exp := range c.seen {
			if now.After(exp) {
				delete(c.seen, k)
			}
		}
		c.sweepAt = now.Add(nonceTTL)
	}
	if exp, ok := c.seen[nonce]; ok && now.Before(exp) {
		return false
	}
	c.seen[nonce] = now.Add(nonceTTL)
	return true
}
