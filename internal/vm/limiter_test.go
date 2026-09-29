//go:build linux

package vm

import (
	"sync"
	"testing"
	"time"
)

// Guards a data race on m.createLimiter and a release draining a different
// channel than it acquired. Run with -race.
func TestAcquireCreateSlotConcurrentResize(t *testing.T) {
	m := &Manager{createLimiter: make(chan struct{}, 4)}

	var wg sync.WaitGroup
	// Resizers churn the channel underneath the acquirers.
	for _, n := range []int{1, 8, 2, 16, 4} {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				m.SetMaxConcurrent(n)
			}
		}(n)
	}
	// Every successful acquire must return a release that neither panics nor blocks.
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				if release, ok := m.acquireCreateSlot(); ok {
					release()
				}
			}
		}()
	}
	wg.Wait()
}

func TestAcquireCreateSlotFullRejects(t *testing.T) {
	old := createSlotWait
	createSlotWait = 20 * time.Millisecond
	t.Cleanup(func() { createSlotWait = old })
	m := &Manager{createLimiter: make(chan struct{}, 1)}

	release, ok := m.acquireCreateSlot()
	if !ok {
		t.Fatal("first acquire should succeed")
	}
	if _, ok := m.acquireCreateSlot(); ok {
		t.Fatal("second acquire should be rejected while the slot is held")
	}
	release()
	release2, ok := m.acquireCreateSlot()
	if !ok {
		t.Fatal("acquire should succeed again after release")
	}
	release2()
}

// A create that finds every slot taken queues for one instead of failing.
func TestAcquireCreateSlotWaitsForAFreeSlot(t *testing.T) {
	m := &Manager{createLimiter: make(chan struct{}, 1)}
	release, _ := m.acquireCreateSlot()
	go func() { time.Sleep(50 * time.Millisecond); release() }()
	start := time.Now()
	r2, ok := m.acquireCreateSlot()
	if !ok {
		t.Fatal("a queued create was refused although a slot freed up 50 ms later")
	}
	r2()
	if time.Since(start) < 40*time.Millisecond {
		t.Fatal("the second acquire did not wait for the slot")
	}
}
