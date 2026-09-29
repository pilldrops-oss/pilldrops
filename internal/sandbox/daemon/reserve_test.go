package daemon

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/runtime/runtimetest"
)

func newReserveDaemon() *Daemon {
	return New(Config{Runtime: runtimetest.NewFake(), IdleTimeout: time.Minute, HardTTL: time.Hour})
}

// Exactly cap reservations may succeed under contention.
func TestReserveConcurrencyRace(t *testing.T) {
	d := newReserveDaemon()
	const max = 3
	const goroutines = 64

	var granted int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // maximize contention
			// Never released, like an in-flight create.
			if _, ok := d.ReserveConcurrency("acme", max); ok {
				atomic.AddInt64(&granted, 1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if granted != max {
		t.Fatalf("granted %d reservations at cap %d, want exactly %d", granted, max, max)
	}
}

func TestReserveConcurrencyReleaseFreesSlot(t *testing.T) {
	d := newReserveDaemon()

	r1, ok := d.ReserveConcurrency("acme", 1)
	if !ok {
		t.Fatal("first reservation should succeed")
	}
	if _, ok := d.ReserveConcurrency("acme", 1); ok {
		t.Fatal("second reservation should be rejected at cap 1")
	}
	r1()
	r2, ok := d.ReserveConcurrency("acme", 1)
	if !ok {
		t.Fatal("reservation should succeed after the first is released")
	}
	r2()
	r2() // a second release must not free an extra slot

	a, ok := d.ReserveConcurrency("acme", 1)
	if !ok {
		t.Fatal("slot should be available after release")
	}
	if _, ok := d.ReserveConcurrency("acme", 1); ok {
		t.Fatal("double release must not have freed two slots")
	}
	a()
}

func TestReserveConcurrencyUnlimited(t *testing.T) {
	d := newReserveDaemon()
	for i := 0; i < 100; i++ {
		if _, ok := d.ReserveConcurrency("acme", 0); !ok {
			t.Fatalf("reservation %d rejected under an uncapped plan", i)
		}
	}
}
