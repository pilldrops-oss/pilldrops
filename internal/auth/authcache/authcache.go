// Package authcache memoizes API-key validation so an authenticated request
// does not pay a database round trip. It is separate from aat because
// internal/store imports aat, which would be an import cycle.
package authcache

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// TTL bounds how long a validated key is trusted without re-checking the
// store. A key revoked by another process keeps working here for up to this long.
const TTL = 60 * time.Second

// Max bounds entries; the cache is emptied when it is reached.
const Max = 4096

type entry struct {
	key store.Key
	exp time.Time
}

// Cache memoizes ValidateKey results keyed by a hash of the presented token.
// Safe for concurrent use; call New.
type Cache struct {
	mu sync.RWMutex
	m  map[string]entry
}

func New() *Cache { return &Cache{m: map[string]entry{}} }

// Key digests a plaintext token into the cache key for Get and Put.
func Key(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// Get returns a cached key for a digest from Key, or ok=false when absent or expired.
func (c *Cache) Get(hash string) (store.Key, bool) {
	c.mu.RLock()
	e, ok := c.m[hash]
	c.mu.RUnlock()
	if !ok || time.Now().After(e.exp) {
		return store.Key{}, false
	}
	return e.key, true
}

// Put records a validated key against a digest from Key.
func (c *Cache) Put(hash string, k store.Key) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= Max {
		c.m = map[string]entry{}
	}
	c.m[hash] = entry{key: k, exp: time.Now().Add(TTL)}
}

// Clear drops every entry so a revoke handled by this process takes effect immediately.
func (c *Cache) Clear() {
	c.mu.Lock()
	c.m = map[string]entry{}
	c.mu.Unlock()
}
