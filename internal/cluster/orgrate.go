package cluster

import (
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// OrgRate caps one org's requests per second across every orchestrator
// replica sharing a Redis, so its limit no longer multiplies with the number
// of nodes (each node limits only what reaches it) or replicas.
//
// ponytail: fixed one-second window, so a burst straddling a boundary can pass
// 2x the limit; a sliding window or GCRA if that ever matters.
type OrgRate struct {
	limit int64
	c     *redis.Client // nil: one replica, counted in process

	mu    sync.Mutex
	local map[string]orgWindow
}

type orgWindow struct {
	sec int64
	n   int64
}

// NewOrgRate builds a limiter of perSec requests per org; perSec <= 0 disables
// it. An empty redisURL counts in this process only.
func NewOrgRate(perSec int, redisURL string) (*OrgRate, error) {
	o := &OrgRate{limit: int64(perSec), local: map[string]orgWindow{}}
	if perSec <= 0 || redisURL == "" {
		return o, nil
	}
	c, err := dialRedis(redisURL)
	if err != nil {
		return nil, err
	}
	o.c = c
	return o, nil
}

// Allow counts one request for org and reports whether it is within the limit.
// A Redis failure allows: the nodes' own per-org limits still stand behind it.
func (o *OrgRate) Allow(org string, now time.Time) bool {
	if o == nil || o.limit <= 0 || org == "" {
		return true
	}
	sec := now.Unix()
	if o.c == nil {
		o.mu.Lock()
		defer o.mu.Unlock()
		w := o.local[org]
		if w.sec != sec {
			w = orgWindow{sec: sec}
		}
		w.n++
		o.local[org] = w
		if len(o.local) > 100_000 { // idle orgs; every window is stale after a second anyway
			o.local = map[string]orgWindow{org: w}
		}
		return w.n <= o.limit
	}
	ctx, cancel := rctx()
	defer cancel()
	key := "sandforge:rl:" + org + ":" + strconv.FormatInt(sec, 10)
	pipe := o.c.TxPipeline()
	incr := pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, 2*time.Second)
	if _, err := pipe.Exec(ctx); err != nil {
		slog.Warn("org rate limit unavailable; allowing", "component", "orgrate", "err", err)
		return true
	}
	return incr.Val() <= o.limit
}
