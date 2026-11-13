package webhook

import (
	"math"
	"math/rand"
	"net/http"
	"time"
)

const (
	// MaxAttempts includes the first try.
	MaxAttempts = 6
	// Backoff schedule: 1s, 2s, 4s, capped at 2 minutes.
	baseBackoff = 1 * time.Second
	maxBackoff  = 2 * time.Minute
	// AutoDisableAfter is the consecutive failure count that switches a hook off.
	// Consecutive, not cumulative, or every long-lived hook would eventually be disabled.
	AutoDisableAfter = 20
)

// Backoff returns the delay before retry number attempt (1-based). Full jitter,
// because a down receiver fails every delivery at once and an unjittered schedule
// would herd them all onto its recovery.
func Backoff(attempt int, rnd *rand.Rand) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := time.Duration(float64(baseBackoff) * math.Pow(2, float64(attempt-1)))
	if d > maxBackoff || d <= 0 {
		d = maxBackoff
	}
	if rnd == nil {
		return d
	}
	// Uniform in (0, d].
	j := time.Duration(rnd.Int63n(int64(d)) + 1)
	if j <= 0 {
		j = time.Millisecond
	}
	return j
}

// Retryable reports whether a response status is worth trying again. 4xx is
// terminal except 408 and 429; 5xx and anything unexpected retries.
func Retryable(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	}
	if status >= 400 && status < 500 {
		return false
	}
	return true
}

// Terminal reports whether a delivery should stop after this outcome.
func Terminal(status int, attempt int) bool {
	if !Retryable(status) {
		return true
	}
	return attempt >= MaxAttempts
}
