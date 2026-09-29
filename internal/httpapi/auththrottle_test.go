package httpapi

import "testing"

func TestAuthFailLimiterBlocksAfterBurst(t *testing.T) {
	rl := newRateLimiter(1, 3)
	defer rl.close()
	const ip = "203.0.113.7"

	if rl.blocked(ip) {
		t.Fatal("blocked before any failure")
	}
	for i := 0; i < 3; i++ {
		if rl.blocked(ip) {
			t.Fatalf("blocked at failure %d, still within burst", i)
		}
		rl.allow(ip)
	}
	if !rl.blocked(ip) {
		t.Fatal("not blocked after burst exhausted")
	}
	if rl.blocked("198.51.100.1") {
		t.Fatal("unrelated IP blocked")
	}
}

func TestAuthFailLimiterDisabledIsInert(t *testing.T) {
	rl := newRateLimiter(0, 10)
	defer rl.close()
	for i := 0; i < 1000; i++ {
		rl.allow("203.0.113.7")
	}
	if rl.blocked("203.0.113.7") {
		t.Fatal("disabled limiter blocked a source — would break normal traffic")
	}
}
