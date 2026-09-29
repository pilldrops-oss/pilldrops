package cluster

import (
	"context"
	"os"
	"testing"
	"time"
)

// A revocation published by one replica reaches another's subscriber.
func TestRevocationReachesOtherReplicas(t *testing.T) {
	url := os.Getenv("SANDBOXD_TEST_REDIS_URL")
	if url == "" {
		t.Skip("SANDBOXD_TEST_REDIS_URL not set")
	}
	pub, err := NewRevocations(url)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := NewRevocations(url)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan struct{}, 1)
	go sub.Subscribe(ctx, func() { got <- struct{}{} })
	// The subscription is asynchronous: publish until it is heard.
	deadline := time.After(5 * time.Second)
	for {
		pub.Publish()
		select {
		case <-got:
			return
		case <-deadline:
			t.Fatal("the other replica never heard the revocation")
		case <-time.After(100 * time.Millisecond):
		}
	}
}
