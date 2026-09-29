package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/pilldrops-oss/pilldrops/internal/config"
	"github.com/pilldrops-oss/pilldrops/internal/httpx"
)

// Defaults for the server's protective limits; env-overridable, 0 disables.
const (
	defaultMaxActive = 100 // concurrent sandboxes; 0 = unlimited
	defaultRateRPS   = 20  // requests/sec per API key; 0 = disabled
	defaultRateBurst = 40  // token-bucket burst per API key
)

// rateLimiter is a per-key token-bucket limiter with idle eviction.
type rateLimiter struct {
	rps   rate.Limit
	burst int

	mu       sync.Mutex
	buckets  map[string]*bucket
	stop     chan struct{}
	stopOnce sync.Once
}

type bucket struct {
	lim  *rate.Limiter
	seen time.Time
}

func newRateLimiter(rps float64, burst int) *rateLimiter {
	rl := &rateLimiter{
		rps:     rate.Limit(rps),
		burst:   burst,
		buckets: make(map[string]*bucket),
		stop:    make(chan struct{}),
	}
	if rps > 0 {
		go rl.evict()
	}
	return rl
}

// allow reports whether a request for key may proceed. A disabled limiter always allows.
func (rl *rateLimiter) allow(key string) bool {
	if rl == nil || rl.rps <= 0 {
		return true
	}
	rl.mu.Lock()
	b, ok := rl.buckets[key]
	if !ok {
		b = &bucket{lim: rate.NewLimiter(rl.rps, rl.burst)}
		rl.buckets[key] = b
	}
	b.seen = time.Now()
	rl.mu.Unlock()
	return b.lim.Allow()
}

// blocked reports whether key has no tokens left without consuming one, so the
// auth path can reject an over-budget source before validating anything.
func (rl *rateLimiter) blocked(key string) bool {
	if rl == nil || rl.rps <= 0 {
		return false
	}
	rl.mu.Lock()
	b, ok := rl.buckets[key]
	rl.mu.Unlock()
	if !ok {
		return false
	}
	return b.lim.Tokens() < 1
}

// evict drops buckets unused for 10 minutes so the map cannot grow without bound.
func (rl *rateLimiter) evict() {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-rl.stop:
			return
		case <-t.C:
			cutoff := time.Now().Add(-10 * time.Minute)
			rl.mu.Lock()
			for k, b := range rl.buckets {
				if b.seen.Before(cutoff) {
					delete(rl.buckets, k)
				}
			}
			rl.mu.Unlock()
		}
	}
}

func (rl *rateLimiter) close() {
	if rl == nil {
		return
	}
	rl.stopOnce.Do(func() { close(rl.stop) })
}

// throttleByIP wraps a handler with a per-client-IP token bucket.
func (s *Server) throttleByIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.previewLimiter.allow(httpx.ClientIP(r)) {
			w.Header().Set("Retry-After", "1")
			writeErr(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// envInt, envStr and envBool delegate to internal/config, which records a
// present-but-unparseable value so startup can refuse it.
func envInt(key string, def int) int { return config.Int(key, def) }

func envStr(key, def string) string { return config.Str(key, def) }

// envBool accepts "true" as well as "1"; the old envInt idiom did not.
func envBool(key string, def bool) bool { return config.Bool(key, def) }

// splitCSV parses a comma-separated env value into trimmed, non-empty parts.
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// decodeBody decodes r.Body into v. An empty body is fine; a malformed one returns false.
func decodeBody(r *http.Request, v any) bool {
	err := json.NewDecoder(r.Body).Decode(v)
	return err == nil || errors.Is(err, io.EOF)
}
