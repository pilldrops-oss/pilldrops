package cluster

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// Old project name kept on purpose: renaming orphans the allocated name set,
	// so two boxes could get the same NODE_ID.
	joinTokenPrefix = "sandforge:join:"
	joinNamesKey    = "sandforge:node-names"
	joinSeqKey      = "sandforge:node-seq"
)

type redisJoinStore struct {
	c *redis.Client
}

func NewRedisJoinStore(url string) (*redisJoinStore, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, err
	}
	return &redisJoinStore{c: redis.NewClient(opt)}, nil
}

func (r *redisJoinStore) Mint(now time.Time, ttl time.Duration) string {
	tok := randToken()
	r.c.Set(context.Background(), joinTokenPrefix+tok, "1", ttl)
	return tok
}

func (r *redisJoinStore) Consume(token string, _ time.Time) bool {
	// GETDEL is atomic and the TTL enforces expiry, so a hit means live and first.
	v, err := r.c.GetDel(context.Background(), joinTokenPrefix+token).Result()
	return err == nil && v == "1"
}

func (r *redisJoinStore) AllocateName(suggested string, taken []string) string {
	ctx := context.Background()
	// Claim names already live in the registry so auto-assignment cannot collide with a running node.
	if len(taken) > 0 {
		args := make([]any, len(taken))
		for i, n := range taken {
			args[i] = n
		}
		r.c.SAdd(ctx, joinNamesKey, args...)
	}
	try := func(name string) bool {
		// SAdd returns 1 only if newly added, so this is an atomic claim.
		n, err := r.c.SAdd(ctx, joinNamesKey, name).Result()
		return err == nil && n == 1
	}
	if suggested != "" {
		if try(suggested) {
			return suggested
		}
		for i := 2; ; i++ {
			cand := fmt.Sprintf("%s-%d", suggested, i)
			if try(cand) {
				return cand
			}
		}
	}
	for {
		seq, err := r.c.Incr(ctx, joinSeqKey).Result()
		if err != nil {
			seq = time.Now().UnixNano()
		}
		cand := fmt.Sprintf("node-%d", seq)
		if try(cand) {
			return cand
		}
	}
}
