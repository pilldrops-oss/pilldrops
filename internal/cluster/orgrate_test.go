package cluster

import (
	"os"
	"testing"
	"time"
)

func TestOrgRateLocal(t *testing.T) {
	o, _ := NewOrgRate(3, "")
	now := time.Unix(1000, 0)
	for i := 0; i < 3; i++ {
		if !o.Allow("a", now) {
			t.Fatalf("request %d refused under the limit", i+1)
		}
	}
	if o.Allow("a", now) {
		t.Fatal("the 4th request in one second passed a limit of 3")
	}
	if !o.Allow("b", now) {
		t.Fatal("one org's traffic limited another")
	}
	if !o.Allow("a", now.Add(time.Second)) {
		t.Fatal("the next second did not start a fresh window")
	}
	off, _ := NewOrgRate(0, "")
	for i := 0; i < 100; i++ {
		if !off.Allow("a", now) {
			t.Fatal("a disabled limiter refused")
		}
	}
}

// Two replicas on one Redis share the count: that is the point of it.
func TestOrgRateSharedAcrossReplicas(t *testing.T) {
	url := os.Getenv("SANDBOXD_TEST_REDIS_URL")
	if url == "" {
		t.Skip("SANDBOXD_TEST_REDIS_URL not set")
	}
	a, err := NewOrgRate(4, url)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewOrgRate(4, url)
	now := time.Unix(time.Now().Unix()+10_000, 0) // a window no other run used
	org := "org-" + now.Format("150405.000000000")
	for i := 0; i < 2; i++ {
		if !a.Allow(org, now) || !b.Allow(org, now) {
			t.Fatalf("round %d refused under the shared limit", i+1)
		}
	}
	if a.Allow(org, now) {
		t.Fatal("replica a passed a 5th request: the replicas are not sharing one count")
	}
}
