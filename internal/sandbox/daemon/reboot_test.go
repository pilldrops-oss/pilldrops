package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/runtimetest"
)

// rebootRT is a runtime after a host reboot: every container still exists,
// stopped, and Adopt hands each back as a casualty to start again.
type rebootRT struct {
	*runtimetest.Fake
	stopped map[string]*casualtyVMM
	swept   [][]string
}

var _ rt.Adopter = (*rebootRT)(nil)

func (r *rebootRT) Adopt(_ context.Context, id string) (rt.Instance, error) {
	if c, ok := r.stopped[id]; ok {
		return c, nil
	}
	return nil, rt.ErrNotFound
}

func (r *rebootRT) SweepUnknown(known []string) { r.swept = append(r.swept, known) }

// rebooted builds a runtime holding one stopped container per id; ids in
// wontStart fail their start.
func rebooted(t *testing.T, ids []string, wontStart map[string]bool) *rebootRT {
	t.Helper()
	r := &rebootRT{Fake: runtimetest.NewFake(), stopped: map[string]*casualtyVMM{}}
	for _, id := range ids {
		inst, err := r.Boot(context.Background(), rt.BootSpec{SandboxID: id})
		if err != nil {
			t.Fatal(err)
		}
		c := &casualtyVMM{deadVMM: deadVMM{inst}, revived: inst}
		if wontStart[id] {
			c.revived, c.reviveErr = nil, fmt.Errorf("docker start: %w", rt.ErrWontStayUp)
		}
		r.stopped[id] = c
	}
	return r
}

func TestRebootRestoresEveryStoppedContainerAndRemovesNothing(t *testing.T) {
	standby := runningRec("sb-standby", time.Now().Add(time.Hour))
	standby.State = string(StateStandby)
	// Paused, resumed, then rebooted: the record still names the old snapshot.
	resumed := runningRec("sb-resumed", time.Now().Add(time.Hour))
	resumed.Snapshot = "/snap/sb-resumed"
	failed := runningRec("sb-failed", time.Now().Add(time.Hour))
	failed.State = string(StateFailed)
	recs := []PersistedSandbox{runningRec("sb-a", time.Now().Add(time.Hour)), standby, resumed, failed}
	ids := []string{"sb-a", "sb-standby", "sb-resumed", "sb-failed"}

	r := rebooted(t, ids, nil)
	fp := newFakePersister(recs)
	d := New(Config{Runtime: r, Store: fp, SnapshotDir: t.TempDir(), IdleTimeout: time.Hour, HardTTL: time.Hour})
	d.Start()
	defer d.Stop()
	d.revives.Wait()

	for _, id := range ids {
		info, ok := d.Get(id)
		if !ok {
			t.Errorf("%s: dropped after the reboot; its container held its data", id)
			continue
		}
		if info.State != StateRunning {
			t.Errorf("%s: state = %s, want running", id, info.State)
		}
		if r.stopped[id].stopped {
			t.Errorf("%s: its container was removed", id)
		}
		if fp.deleted[id] {
			t.Errorf("%s: its record was deleted", id)
		}
	}
	if fp.countEvents("restarted") != len(ids) {
		t.Errorf("events = %v, want one restarted per sandbox", fp.events)
	}
	if len(r.swept) != 1 || len(r.swept[0]) != len(ids) {
		t.Fatalf("sweep known sets = %v, want all %d sandboxes known", r.swept, len(ids))
	}
}

func TestRebootDestroysAContainerThatWillNotStart(t *testing.T) {
	r := rebooted(t, []string{"sb-broken"}, map[string]bool{"sb-broken": true})
	fp := newFakePersister([]PersistedSandbox{runningRec("sb-broken", time.Now().Add(time.Hour))})
	d := New(Config{Runtime: r, Store: fp, SnapshotDir: t.TempDir(), IdleTimeout: time.Hour, HardTTL: time.Hour})
	d.Start()
	defer d.Stop()
	d.revives.Wait()

	if _, ok := d.Get("sb-broken"); ok {
		t.Fatal("a container that would not start is still listed; a crash is destroyed")
	}
	if !fp.deleted["sb-broken"] {
		t.Error("its record was kept")
	}
	if fp.countEvents("exited") != 1 {
		t.Errorf("events = %v, want one exited", fp.events)
	}
}

// slowStartVMM is a stopped container whose start takes a while and gives up
// when its context ends, as retryStart does.
type slowStartVMM struct {
	deadVMM
	took time.Duration
	// deaf ignores cancellation, as a docker start wedged in the engine does.
	deaf bool
}

func (*slowStartVMM) Casualty() bool           { return true }
func (*slowStartVMM) EngineStarted() time.Time { return time.Time{} }
func (s *slowStartVMM) Revive(ctx context.Context) (rt.Instance, error) {
	if s.deaf {
		time.Sleep(s.took)
		return s.Instance, nil
	}
	select {
	case <-time.After(s.took):
		return s.Instance, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type slowRebootRT struct {
	*runtimetest.Fake
	stopped map[string]*slowStartVMM
}

var _ rt.Adopter = (*slowRebootRT)(nil)

func (r *slowRebootRT) Adopt(_ context.Context, id string) (rt.Instance, error) {
	if c, ok := r.stopped[id]; ok {
		return c, nil
	}
	return nil, rt.ErrNotFound
}

// The revives ran one after another on the restore's own deadline, so on a
// node with a few dozen sandboxes the later ones failed for want of time and
// healthy containers were marked failed.
func TestRebootRevivesEverySandboxWhenTheirStartsOutlastTheRestoreBudget(t *testing.T) {
	defer func(b time.Duration) { restoreBudget = b }(restoreBudget)
	restoreBudget = 100 * time.Millisecond

	r, recs := slowReboot(t, 24, 60*time.Millisecond, false)
	fp := newFakePersister(recs)
	d := New(Config{Runtime: r, Store: fp, SnapshotDir: t.TempDir(), IdleTimeout: time.Hour, HardTTL: time.Hour})
	d.Start()
	defer d.Stop()
	d.revives.Wait()

	for _, rec := range recs {
		if info, ok := d.Get(rec.ID); !ok || info.State != StateRunning {
			t.Errorf("%s: state = %s (present %v), want running", rec.ID, info.State, ok)
		}
	}
	if n := fp.countEvents("exited"); n != 0 {
		t.Errorf("%d healthy containers were marked failed", n)
	}
}

// listFailRT cannot list its instances until listing is set, as while dockerd
// is still starting after a reboot.
type listFailRT struct {
	*rebootRT
	listing atomic.Bool
}

func (r *listFailRT) Adopt(ctx context.Context, id string) (rt.Instance, error) {
	if !r.listing.Load() {
		return nil, errors.New("docker ps: Cannot connect to the Docker daemon")
	}
	return r.rebootRT.Adopt(ctx, id)
}

func TestRestoreDecidesNothingFromAFailedListingAndRetries(t *testing.T) {
	plain := runningRec("sb-plain", time.Now().Add(time.Hour))
	snapped := runningRec("sb-snapped", time.Now().Add(time.Hour))
	snapped.Snapshot = "/snap/sb-snapped"
	expired := runningRec("sb-expired", time.Now().Add(-time.Minute))
	r := &listFailRT{rebootRT: rebooted(t, []string{"sb-plain", "sb-snapped", "sb-expired"}, nil)}
	fp := newFakePersister([]PersistedSandbox{expired, plain, snapped})
	d := New(Config{Runtime: r, Store: fp, SnapshotDir: t.TempDir(), IdleTimeout: time.Hour, HardTTL: time.Hour})
	d.Start()
	defer d.Stop()
	d.revives.Wait()

	fp.mu.Lock()
	deleted := len(fp.deleted)
	fp.mu.Unlock()
	if deleted != 0 {
		t.Errorf("records dropped on a failed listing: %v", fp.deleted)
	}
	if _, ok := d.Get("sb-snapped"); ok {
		t.Error("sb-snapped came back from its snapshot while its container could not be looked up")
	}
	if len(r.swept) != 0 {
		t.Errorf("swept %v on a failed listing", r.swept)
	}

	r.listing.Store(true)
	deadline := time.Now().Add(5 * time.Second)
	for {
		p, ok1 := d.Get("sb-plain")
		s, ok2 := d.Get("sb-snapped")
		if ok1 && ok2 && p.State == StateRunning && s.State == StateRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not restored once listing worked: plain=%v snapped=%v", p.State, s.State)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !r.stopped["sb-expired"].stopped {
		t.Error("the expired sandbox's container was not removed once it could be looked up")
	}
}

// slowReboot builds n stopped containers whose start takes took.
func slowReboot(t *testing.T, n int, took time.Duration, deaf bool) (*slowRebootRT, []PersistedSandbox) {
	t.Helper()
	r := &slowRebootRT{Fake: runtimetest.NewFake(), stopped: map[string]*slowStartVMM{}}
	var recs []PersistedSandbox
	for i := range n {
		id := "sb-" + string(rune('a'+i))
		inst, err := r.Boot(context.Background(), rt.BootSpec{SandboxID: id})
		if err != nil {
			t.Fatal(err)
		}
		r.stopped[id] = &slowStartVMM{deadVMM: deadVMM{inst}, took: took, deaf: deaf}
		recs = append(recs, runningRec(id, time.Now().Add(time.Hour)))
	}
	return r, recs
}

// Start held the HTTP listener and heartbeat until every revive finished; on a
// node full of slow starts the control plane reaped the node meanwhile.
func TestStartReturnsWhileRevivesRun(t *testing.T) {
	r, recs := slowReboot(t, 8, 400*time.Millisecond, false)
	fp := newFakePersister(recs)
	d := New(Config{Runtime: r, Store: fp, SnapshotDir: t.TempDir(), IdleTimeout: time.Hour, HardTTL: time.Hour})
	t0 := time.Now()
	d.Start()
	defer d.Stop()
	if took := time.Since(t0); took > 200*time.Millisecond {
		t.Errorf("Start took %v; it waited for the revives", took)
	}
	d.revives.Wait()
	if n := fp.countEvents("restarted"); n != len(recs) {
		t.Errorf("restarted events = %d, want %d once the revives finished", n, len(recs))
	}
}

func TestStopDuringRevivesIsBounded(t *testing.T) {
	defer func(w time.Duration) { reviveStopWait = w }(reviveStopWait)
	reviveStopWait = 200 * time.Millisecond
	for _, deaf := range []bool{false, true} {
		r, recs := slowReboot(t, 4, 5*time.Second, deaf)
		fp := newFakePersister(recs)
		d := New(Config{Runtime: r, Store: fp, SnapshotDir: t.TempDir(), IdleTimeout: time.Hour, HardTTL: time.Hour})
		d.Start()
		time.Sleep(50 * time.Millisecond) // the revives are in flight
		t0 := time.Now()
		d.Stop()
		if took := time.Since(t0); took > time.Second {
			t.Errorf("deaf=%v: Stop took %v with revives in flight, want at most ~%v", deaf, took, reviveStopWait)
		}
		if !deaf {
			for _, rec := range recs {
				if info, ok := d.Get(rec.ID); !ok || info.State == StateFailed {
					t.Errorf("%s: a revive cancelled by Stop marked the sandbox failed or dropped it", rec.ID)
				}
			}
		}
	}
}

// partListRT cannot look up the ids in failing, as when dockerd answers some
// calls and not others while it starts.
type partListRT struct {
	*rebootRT
	failing map[string]bool
}

func (r *partListRT) Adopt(ctx context.Context, id string) (rt.Instance, error) {
	if r.failing[id] {
		return nil, errors.New("docker ps: Cannot connect to the Docker daemon")
	}
	return r.rebootRT.Adopt(ctx, id)
}

// A retry pass works from rows it read before a destroy that came after the
// first pass restored the sandbox; it must not bring the sandbox back.
func TestRestoreRetryDoesNotResurrectADestroyedSandbox(t *testing.T) {
	r := &partListRT{rebootRT: rebooted(t, []string{"sb-a", "sb-b"}, nil), failing: map[string]bool{"sb-b": true}}
	// The fake store keeps returning both rows: the retry's read predates the destroy.
	fp := newFakePersister([]PersistedSandbox{
		runningRec("sb-a", time.Now().Add(time.Hour)), runningRec("sb-b", time.Now().Add(time.Hour)),
	})
	d := New(Config{Runtime: r, Store: fp, SnapshotDir: t.TempDir(), IdleTimeout: time.Hour, HardTTL: time.Hour})
	ctx := context.Background()
	if d.restoreFromStore(ctx) {
		t.Fatal("the first pass finished although sb-b could not be looked up")
	}
	d.revives.Wait()
	if err := d.Destroy(ctx, "sb-a"); err != nil {
		t.Fatalf("destroy: %v", err)
	}

	r.failing = nil
	if !d.restoreFromStore(ctx) {
		t.Fatal("the retry did not finish")
	}
	d.revives.Wait()
	if _, ok := d.Get("sb-a"); ok {
		t.Error("the retry brought back a sandbox destroyed after the first pass")
	}
	if _, ok := d.Get("sb-b"); !ok {
		t.Error("the retry did not restore sb-b")
	}
}

func TestRestoreRetryBacksOff(t *testing.T) {
	var b restoreRetry
	now := time.Now()
	var waits []time.Duration
	for range 8 {
		b.failed(now)
		waits = append(waits, b.next.Sub(now))
		if b.due(now) || !b.due(b.next) {
			t.Fatalf("due before or not at the scheduled retry (wait %v)", b.wait)
		}
		now = b.next
	}
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30, 30}
	for i, w := range want {
		if waits[i] != w*time.Second {
			t.Fatalf("waits = %v, want %v seconds", waits, want)
		}
	}
}
