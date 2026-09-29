package cluster

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Locks are short-lived named locks shared by every orchestrator replica on one
// Redis, or held in this process when there is none. They guard operations
// that must not overlap on one sandbox, such as a move and a destroy.
type Locks struct {
	c *redis.Client // nil: one replica, locks live in process

	mu    sync.Mutex
	local map[string]time.Time
}

// NewLocks uses the Redis at url, or this process alone when url is empty.
func NewLocks(url string) (*Locks, error) {
	l := &Locks{local: map[string]time.Time{}}
	if url == "" {
		return l, nil
	}
	c, err := dialRedis(url)
	if err != nil {
		return nil, err
	}
	l.c = c
	return l, nil
}

// unlockScript deletes the lock only if this holder still owns it, so a holder
// whose lock expired cannot release the next holder's.
var unlockScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end
return 0`)

// TryLock takes name for up to ttl. ok is false when someone else holds it.
func (l *Locks) TryLock(name string, ttl time.Duration) (release func(), ok bool) {
	key := "sandforge:lock:" + name
	if l.c == nil {
		l.mu.Lock()
		defer l.mu.Unlock()
		if until, held := l.local[key]; held && time.Now().Before(until) {
			return nil, false
		}
		l.local[key] = time.Now().Add(ttl)
		return func() { l.mu.Lock(); delete(l.local, key); l.mu.Unlock() }, true
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	token := hex.EncodeToString(b[:])
	ctx, cancel := rctx()
	defer cancel()
	got, err := l.c.SetNX(ctx, key, token, ttl).Result()
	if err != nil || !got {
		return nil, false
	}
	return func() {
		ctx, cancel := rctx()
		defer cancel()
		_ = unlockScript.Run(ctx, l.c, []string{key}, token).Err()
	}, true
}

// Held reports whether name is locked right now.
func (l *Locks) Held(name string) bool {
	key := "sandforge:lock:" + name
	if l.c == nil {
		l.mu.Lock()
		defer l.mu.Unlock()
		until, held := l.local[key]
		return held && time.Now().Before(until)
	}
	ctx, cancel := rctx()
	defer cancel()
	n, err := l.c.Exists(ctx, key).Result()
	return err == nil && n > 0
}
