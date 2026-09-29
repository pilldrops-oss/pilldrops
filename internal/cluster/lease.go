package cluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/redis/go-redis/v9"
)

// NewReplicaID returns a random identity for this orchestrator replica.
func NewReplicaID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "orch-" + hex.EncodeToString(b)
}

// LeaderLease is a best-effort Redis lease so one replica runs the reconciler
// at a time. Best-effort suffices because reconciler passes are idempotent.
type LeaderLease struct {
	c   *redis.Client
	key string
	id  string
	ttl time.Duration
}

func NewLeaderLease(url, key, id string, ttl time.Duration) (*LeaderLease, error) {
	c, err := dialRedis(url)
	if err != nil {
		return nil, err
	}
	return &LeaderLease{c: c, key: key, id: id, ttl: ttl}, nil
}

// IsLeader acquires the lease if free or renews it if held by this replica.
func (l *LeaderLease) IsLeader(ctx context.Context) bool {
	if ok, err := l.c.SetNX(ctx, l.key, l.id, l.ttl).Result(); err == nil && ok {
		return true
	}
	if cur, err := l.c.Get(ctx, l.key).Result(); err == nil && cur == l.id {
		l.c.PExpire(ctx, l.key, l.ttl)
		return true
	}
	return false
}

// Release drops the lease if this replica holds it. No-op otherwise.
func (l *LeaderLease) Release(ctx context.Context) error {
	// Compare-and-delete so a successor's lease is never removed.
	const script = `if redis.call("get", KEYS[1]) == ARGV[1] then return redis.call("del", KEYS[1]) else return 0 end`
	return l.c.Eval(ctx, script, []string{l.key}, l.id).Err()
}
