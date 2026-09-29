package cluster

import (
	"context"
	"os"
	"testing"
	"time"
)

// redisTestURL returns the Redis to test against, or skips.
func redisTestURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("SANDBOXD_TEST_REDIS_URL")
	if url == "" {
		url = os.Getenv("REDIS_TEST_URL")
	}
	if url == "" {
		t.Skip("set SANDBOXD_TEST_REDIS_URL to run Redis JoinStore tests")
	}
	return url
}

func TestRedisJoinStoreRoundtrip(t *testing.T) {
	js, err := NewRedisJoinStore(redisTestURL(t))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	now := time.Now()

	tok := js.Mint(now, 15*time.Minute)
	if !js.Consume(tok, now) {
		t.Fatal("first consume should succeed")
	}
	if js.Consume(tok, now) {
		t.Fatal("second consume must fail (one-time)")
	}

	a := js.AllocateName("", nil)
	b := js.AllocateName("", nil)
	if a == b {
		t.Fatalf("names collided: %q == %q", a, b)
	}
	if got := js.AllocateName(a, nil); got == a {
		t.Fatalf("taken suggestion %q returned a duplicate", a)
	}
}

// Legacy nodes absent from the Redis names set must still never be handed out.
func TestRedisAllocateNameHonorsTaken(t *testing.T) {
	js, err := NewRedisJoinStore(redisTestURL(t))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	js.c.Del(context.Background(), joinNamesKey, joinSeqKey)

	taken := []string{"node-1", "node-2"}
	for i := 0; i < 10; i++ {
		got := js.AllocateName("", taken)
		if got == "node-1" || got == "node-2" {
			t.Fatalf("AllocateName returned a pre-existing node id %q", got)
		}
	}
}
