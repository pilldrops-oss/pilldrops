package cluster

import (
	"context"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testAdmission(t *testing.T) *Admission {
	url := os.Getenv("SANDBOXD_TEST_REDIS_URL")
	if url == "" {
		t.Skip("SANDBOXD_TEST_REDIS_URL not set")
	}
	a, err := NewAdmission(url)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// Fifty concurrent creates against a cap of five: exactly five get in.
func TestAdmissionNeverOverAdmits(t *testing.T) {
	a := testAdmission(t)
	org := "org-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := a.Reserve(context.Background(), org, "s"+strconv.Itoa(i), 0, 5, time.Now())
			if err != nil {
				t.Error(err)
			}
			if ok {
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := admitted.Load(); n != 5 {
		t.Fatalf("admitted %d of 50 against a cap of 5", n)
	}
}

// Live sandboxes count, and a released claim keeps its place for the grace
// period, then frees it.
func TestAdmissionLiveCountAndReleaseGrace(t *testing.T) {
	a := testAdmission(t)
	ctx := context.Background()
	org := "org-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	now := time.Now()

	if ok, _ := a.Reserve(ctx, org, "s1", 2, 3, now); !ok {
		t.Fatal("2 live + 0 pending < 3 was refused")
	}
	if ok, _ := a.Reserve(ctx, org, "s2", 2, 3, now); ok {
		t.Fatal("2 live + 1 pending admitted against a cap of 3")
	}
	_ = a.Release(ctx, org, "s1", now)
	if ok, _ := a.Reserve(ctx, org, "s3", 2, 3, now.Add(time.Second)); ok {
		t.Fatal("a released claim stopped counting inside its grace period")
	}
	if ok, _ := a.Reserve(ctx, org, "s4", 2, 3, now.Add(admissionReleaseGrace+time.Second)); !ok {
		t.Fatal("a released claim still counted after its grace period")
	}
}
