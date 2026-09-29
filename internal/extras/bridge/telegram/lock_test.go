package telegram

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// blockingBoxes holds Exec open until released, so a turn can be observed mid-flight.
type blockingBoxes struct {
	entered chan struct{}
	release chan struct{}

	mu       sync.Mutex
	inFlight int
	maxSeen  int
}

func newBlockingBoxes() *blockingBoxes {
	return &blockingBoxes{entered: make(chan struct{}, 8), release: make(chan struct{})}
}

func (b *blockingBoxes) Get(string) (SandboxState, bool) {
	return SandboxState{Template: "claude"}, true
}
func (b *blockingBoxes) Resume(context.Context, string) error { return nil }

func (b *blockingBoxes) Exec(context.Context, string, rt.ExecSpec) (rt.ExecResult, error) {
	b.mu.Lock()
	b.inFlight++
	if b.inFlight > b.maxSeen {
		b.maxSeen = b.inFlight
	}
	b.mu.Unlock()

	b.entered <- struct{}{}
	<-b.release

	b.mu.Lock()
	b.inFlight--
	b.mu.Unlock()
	return rt.ExecResult{Stdout: "ok"}, nil
}

func (b *blockingBoxes) concurrentPeak() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.maxSeen
}

func TestTurnsDoNotOverlapOnOneWorkstation(t *testing.T) {
	boxes := newBlockingBoxes()
	gate := NewSandboxGate()

	first := make(chan error, 1)
	go func() {
		r := &DaemonRunner{Boxes: boxes, Gate: gate}
		_, err := r.Run(context.Background(), "sb_1", "first", RunOpts{})
		first <- err
	}()
	<-boxes.entered // the agent is now running

	r2 := &DaemonRunner{Boxes: boxes, Gate: gate}
	if _, err := r2.Run(context.Background(), "sb_1", "second", RunOpts{}); !errors.Is(err, ErrBusy) {
		t.Fatalf("second concurrent turn: want ErrBusy, got %v", err)
	}

	close(boxes.release)
	if err := <-first; err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if peak := boxes.concurrentPeak(); peak != 1 {
		t.Errorf("two agents ran at once on one workstation (peak %d)", peak)
	}
}

// Proves the harness can observe overlap, so the gate test is not passing vacuously.
func TestHarnessDetectsOverlapWithoutTheGate(t *testing.T) {
	boxes := newBlockingBoxes()

	for i := 0; i < 2; i++ {
		go func() {
			r := &DaemonRunner{Boxes: boxes, Gate: NoGate}
			_, _ = r.Run(context.Background(), "sb_1", "concurrent", RunOpts{})
		}()
	}
	<-boxes.entered
	<-boxes.entered // both are inside Exec at the same time

	if peak := boxes.concurrentPeak(); peak != 2 {
		t.Fatalf("harness cannot see overlap: peak %d, want 2", peak)
	}
	close(boxes.release)
}

func TestDifferentWorkstationsAreIndependent(t *testing.T) {
	boxes := newBlockingBoxes()
	gate := NewSandboxGate()

	go func() {
		r := &DaemonRunner{Boxes: boxes, Gate: gate}
		_, _ = r.Run(context.Background(), "sb_1", "first", RunOpts{})
	}()
	<-boxes.entered

	done := make(chan error, 1)
	go func() {
		r := &DaemonRunner{Boxes: boxes, Gate: gate}
		_, err := r.Run(context.Background(), "sb_2", "other customer", RunOpts{})
		done <- err
	}()
	<-boxes.entered // sb_2 got in while sb_1 is still running

	close(boxes.release)
	if err := <-done; err != nil {
		t.Fatalf("second workstation blocked by the first: %v", err)
	}
	if peak := boxes.concurrentPeak(); peak != 2 {
		t.Errorf("want 2 workstations running concurrently, saw peak %d", peak)
	}
}

func TestSlotIsFreedAfterTurnEnds(t *testing.T) {
	gate := NewSandboxGate()
	boxes := &fakeBoxes{
		state:  SandboxState{Template: "claude"},
		exists: true,
		res:    rt.ExecResult{Stdout: "done"},
	}
	r := &DaemonRunner{Boxes: boxes, Gate: gate}

	for i := 0; i < 3; i++ {
		if _, err := r.Run(context.Background(), "sb_1", "again", RunOpts{}); err != nil {
			t.Fatalf("sequential turn %d: %v", i, err)
		}
	}
}

func TestSlotIsFreedWhenTurnFails(t *testing.T) {
	gate := NewSandboxGate()
	boxes := &fakeBoxes{exists: false} // "no longer exists" path
	r := &DaemonRunner{Boxes: boxes, Gate: gate}

	if _, err := r.Run(context.Background(), "sb_1", "x", RunOpts{}); err == nil {
		t.Fatal("expected the missing-workstation error")
	}
	if !gate.acquire("sb_1") {
		t.Fatal("slot still held after a failed turn")
	}
	gate.release("sb_1")
}

// Production builds a runner per request, so the default gate must be shared across instances.
func TestNilGateIsSharedAcrossRunnerInstances(t *testing.T) {
	release, ok := AcquireTurn("sb_shared")
	if !ok {
		t.Fatal("could not take the slot")
	}
	defer release()

	r := &DaemonRunner{Boxes: &fakeBoxes{exists: true}}
	if _, err := r.Run(context.Background(), "sb_shared", "x", RunOpts{}); !errors.Is(err, ErrBusy) {
		t.Fatalf("separate runner instance did not share the process gate: got %v", err)
	}
}

func TestNoGateDoesNotDoubleTake(t *testing.T) {
	release, ok := AcquireTurn("sb_held")
	if !ok {
		t.Fatal("could not take the slot")
	}
	defer release()

	boxes := &fakeBoxes{
		state:  SandboxState{Template: "claude"},
		exists: true,
		res:    rt.ExecResult{Stdout: "done"},
	}
	r := &DaemonRunner{Boxes: boxes, Gate: NoGate}
	if _, err := r.Run(context.Background(), "sb_held", "x", RunOpts{}); err != nil {
		t.Fatalf("NoGate runner blocked on a slot its caller holds: %v", err)
	}
}

func TestGateDoesNotRetainReleasedWorkstations(t *testing.T) {
	gate := NewSandboxGate()
	for i := 0; i < 100; i++ {
		id := "sb_" + strconv.Itoa(i)
		if !gate.acquire(id) {
			t.Fatalf("acquire %s", id)
		}
		gate.release(id)
	}
	gate.mu.Lock()
	n := len(gate.held)
	gate.mu.Unlock()
	if n != 0 {
		t.Errorf("gate retained %d released workstations", n)
	}
}
