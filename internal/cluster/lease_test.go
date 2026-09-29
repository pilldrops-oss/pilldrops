package cluster

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestLeaderLease(t *testing.T) {
	url := os.Getenv("SANDBOXD_TEST_REDIS_URL")
	if url == "" {
		t.Skip("set SANDBOXD_TEST_REDIS_URL to run the leader-lease test")
	}
	ctx := context.Background()
	key := "sandforge:leader:test"

	a, err := NewLeaderLease(url, key, "replica-a", 2*time.Second)
	if err != nil {
		t.Fatalf("lease a: %v", err)
	}
	b, err := NewLeaderLease(url, key, "replica-b", 2*time.Second)
	if err != nil {
		t.Fatalf("lease b: %v", err)
	}
	_ = a.Release(ctx)

	if !a.IsLeader(ctx) {
		t.Fatal("a should acquire an unheld lease")
	}
	if b.IsLeader(ctx) {
		t.Fatal("b must not be leader while a holds the lease")
	}
	if !a.IsLeader(ctx) {
		t.Fatal("a should renew its own lease")
	}

	if err := a.Release(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
	if !b.IsLeader(ctx) {
		t.Fatal("b should take leadership after a releases")
	}
	_ = b.Release(ctx)
}
