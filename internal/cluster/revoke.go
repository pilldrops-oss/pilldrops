package cluster

import (
	"context"

	"github.com/redis/go-redis/v9"
)

// revokeChannel carries "a credential was revoked": every replica and edge
// drops its auth cache on it, so a revoked key stops working fleet-wide on the
// next request instead of after each replica's cache TTL (measured: 60 s).
const revokeChannel = "sandforge:auth:revoked"

// Revocations broadcasts credential revocations between replicas. A missed
// message (Redis down, a reconnect) leaves the cache TTL as the bound.
type Revocations struct{ c *redis.Client }

// NewRevocations uses the Redis at url; nil, nil when url is empty (one
// replica, whose own cache the revoking request already cleared).
func NewRevocations(url string) (*Revocations, error) {
	if url == "" {
		return nil, nil
	}
	c, err := dialRedis(url)
	if err != nil {
		return nil, err
	}
	return &Revocations{c: c}, nil
}

// Publish tells every subscriber a credential was revoked. Best-effort.
func (r *Revocations) Publish() {
	if r == nil {
		return
	}
	ctx, cancel := rctx()
	defer cancel()
	_ = r.c.Publish(ctx, revokeChannel, "1").Err()
}

// Subscribe calls onRevoke for every revocation until ctx ends. go-redis
// resubscribes on its own after a dropped connection.
func (r *Revocations) Subscribe(ctx context.Context, onRevoke func()) {
	if r == nil {
		return
	}
	sub := r.c.Subscribe(ctx, revokeChannel)
	defer func() { _ = sub.Close() }()
	ch := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ch:
			if !ok {
				return
			}
			onRevoke()
		}
	}
}
