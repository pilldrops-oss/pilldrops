package daemon

import (
	"context"
	"testing"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/runtimetest"
)

// adoptingRT is a fake runtime whose Adopt succeeds only for the ids in adoptable.
type adoptingRT struct {
	*runtimetest.Fake
	adoptable map[string]bool
	adopted   []string
	stops     []string
	swept     [][]string
}

type stopRecorder struct {
	rt.Instance
	rt *adoptingRT
}

func (s *stopRecorder) Stop(ctx context.Context) error {
	s.rt.stops = append(s.rt.stops, s.ID())
	return s.Instance.Stop(ctx)
}

var _ rt.Adopter = (*adoptingRT)(nil)

func (a *adoptingRT) Adopt(ctx context.Context, sandboxID string) (rt.Instance, error) {
	if !a.adoptable[sandboxID] {
		return nil, rt.ErrNotFound
	}
	inst, err := a.Boot(ctx, rt.BootSpec{SandboxID: sandboxID})
	if err != nil {
		return nil, err
	}
	a.adopted = append(a.adopted, sandboxID)
	return &stopRecorder{Instance: inst, rt: a}, nil
}

func (a *adoptingRT) SweepUnknown(known []string) { a.swept = append(a.swept, known) }

func runningRec(id string, expires time.Time) PersistedSandbox {
	return PersistedSandbox{
		ID: id, Template: "python-3.11", OrgID: "acme", State: string(StateRunning),
		VCPU: 1, MemMiB: 512,
		CreatedAt: time.Now().Add(-time.Hour).Unix(), ExpiresAt: expires.Unix(),
	}
}

func TestRestoreAdoptsRunningInstance(t *testing.T) {
	fake := &adoptingRT{Fake: runtimetest.NewFake(), adoptable: map[string]bool{"sb-live": true}}
	fp := newFakePersister([]PersistedSandbox{runningRec("sb-live", time.Now().Add(time.Hour))})
	d := New(Config{
		Runtime: fake, Store: fp, SnapshotDir: t.TempDir(),
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	d.Start()
	defer d.Stop()

	sb, ok := d.Get("sb-live")
	if !ok {
		t.Fatal("running record with a live instance was dropped, want adopted")
	}
	if sb.State != StateRunning {
		t.Fatalf("state = %s, want running (adopted)", sb.State)
	}
	// Exec must reach the live instance.
	if _, err := d.Exec(context.Background(), "sb-live", rt.ExecSpec{Cmd: "echo hi"}); err != nil {
		t.Fatalf("exec on adopted sandbox: %v", err)
	}
}

func TestRestoreDropsRunningWhenNotAdoptable(t *testing.T) {
	fake := &adoptingRT{Fake: runtimetest.NewFake(), adoptable: map[string]bool{}}
	fp := newFakePersister([]PersistedSandbox{runningRec("sb-gone", time.Now().Add(time.Hour))})
	d := New(Config{
		Runtime: fake, Store: fp, SnapshotDir: t.TempDir(),
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	d.Start()
	defer d.Stop()

	if _, ok := d.Get("sb-gone"); ok {
		t.Fatal("unadoptable snapshotless record should be dropped")
	}
}

func TestRestoreStopsExpiredAdoptable(t *testing.T) {
	fake := &adoptingRT{Fake: runtimetest.NewFake(), adoptable: map[string]bool{"sb-exp": true}}
	fp := newFakePersister([]PersistedSandbox{runningRec("sb-exp", time.Now().Add(-time.Minute))})
	d := New(Config{
		Runtime: fake, Store: fp, SnapshotDir: t.TempDir(),
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	d.Start()
	defer d.Stop()

	if _, ok := d.Get("sb-exp"); ok {
		t.Fatal("expired record should be dropped even when adoptable")
	}
	if len(fake.stops) == 0 {
		t.Fatal("expired live instance must be stopped, not leaked")
	}
}

func TestRestoreSweepsUnknownContainers(t *testing.T) {
	fake := &adoptingRT{Fake: runtimetest.NewFake(), adoptable: map[string]bool{"sb-live": true}}
	fp := newFakePersister([]PersistedSandbox{runningRec("sb-live", time.Now().Add(time.Hour))})
	d := New(Config{
		Runtime: fake, Store: fp, SnapshotDir: t.TempDir(),
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	d.Start()
	defer d.Stop()

	if len(fake.swept) != 1 {
		t.Fatalf("SweepUnknown calls = %d, want 1", len(fake.swept))
	}
	if len(fake.swept[0]) != 1 || fake.swept[0][0] != "sb-live" {
		t.Fatalf("swept known set = %v, want [sb-live]", fake.swept[0])
	}
}
