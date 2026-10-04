package orchestrator

import (
	"sync"
	"time"
)

// The unauthenticated on-demand TLS check is rate-limited per source IP at
// 5 rps / burst 10.
//
// Heartbeats used to share that limit, which admitted only ~25 nodes behind one
// address (a node beats every 5 s): past that, measured with a node simulator,
// 90% of beats were 429 and the fleet flapped. One address stands for many
// nodes behind a NAT gateway, and for all of them when this tier sits behind a
// proxy without TRUSTED_PROXY_CIDRS. Heartbeats now get a per-IP ceiling sized
// for ~1000 nodes, still bounding the HMAC work a flood can cause, plus a
// per-node limit applied after the token verifies, so no one can spend a real
// node's allowance. ipLimiter is the package's token bucket; it sweeps idle
// buckets, which matters because handleDomainCheck takes no credential and a
// peer rotating source addresses would otherwise grow the map for the life of
// the process.
const (
	heartbeatRPS   = 5
	heartbeatBurst = 10

	heartbeatIPRPS     = 200
	heartbeatIPBurst   = 400
	heartbeatNodeRPS   = 1
	heartbeatNodeBurst = 5
)

// ipLimiter is a token bucket per client IP, swept opportunistically on write.
type ipLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rps     float64
	burst   float64
	now     func() time.Time // injectable so the tests do not sleep
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newIPLimiter(rps, burst float64) *ipLimiter {
	return &ipLimiter{buckets: map[string]*bucket{}, rps: rps, burst: burst, now: time.Now}
}

func (l *ipLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[ip]
	if !ok {
		// A bucket idle long enough to have refilled is identical to a fresh one.
		if len(l.buckets) > 1024 {
			for k, v := range l.buckets {
				if now.Sub(v.last) > time.Duration(l.burst/l.rps)*time.Second {
					delete(l.buckets, k)
				}
			}
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[ip] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rps)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
