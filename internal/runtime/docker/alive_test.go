package docker

import (
	"context"
	"errors"
	"testing"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

func TestConfirmDeadNeedsTwoReadsApart(t *testing.T) {
	i := &dockerInstance{}
	t0 := time.Now()
	if i.confirmDead(t0) {
		t.Fatal("one stopped read reaped the sandbox")
	}
	if i.confirmDead(t0.Add(time.Second)) {
		t.Fatal("a second read inside the confirmation window reaped it")
	}
	if !i.confirmDead(t0.Add(deadConfirmAfter)) {
		t.Fatal("a container stopped across two reads was not reported dead")
	}
}

// A dockerd restart without live-restore stops every container: before the
// new daemon starts, however long it stayed down (graceful, or an outage), or
// just after it (a crash). On the rig a 60 s outage destroyed every sandbox.
func TestRestartCasualty(t *testing.T) {
	restarted := time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		name     string
		stopped  time.Time
		started  time.Time
		casualty bool
	}{
		{"stopped 10 min before dockerd started (outage)", restarted.Add(-10 * time.Minute), restarted, true},
		{"just before the restart (graceful)", restarted.Add(-time.Second), restarted, true},
		{"stopped 30 s after the start (crash)", restarted.Add(30 * time.Second), restarted, true},
		{"stopped 2 min after the start (slow restore of many)", restarted.Add(2 * time.Minute), restarted, true},
		{"just inside the grace", restarted.Add(restartGrace - time.Second), restarted, true},
		{"at the end of the grace", restarted.Add(restartGrace), restarted, false},
		{"died 10 min into the same dockerd", restarted.Add(10 * time.Minute), restarted, false},
		{"dockerd's start unknown (rootless, snap, remote)", restarted.Add(5 * time.Minute), time.Time{}, true},
		{"stop time unknown", time.Time{}, restarted, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := restartCasualty(tc.stopped, tc.started); got != tc.casualty {
				t.Errorf("casualty = %v, want %v", got, tc.casualty)
			}
		})
	}
}

func TestRetryStartIsBounded(t *testing.T) {
	defer func(b time.Duration) { reviveBackoff = b }(reviveBackoff)
	reviveBackoff = time.Millisecond

	calls := 0
	err := retryStart(context.Background(), func() error { calls++; return errors.New("daemon not ready") })
	if err == nil || calls != reviveTries {
		t.Fatalf("a start that never succeeds: err=%v after %d tries, want an error after %d", err, calls, reviveTries)
	}

	calls = 0
	err = retryStart(context.Background(), func() error {
		calls++
		if calls < 2 {
			return errors.New("daemon not ready")
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("a start that succeeds on the second try: err=%v after %d tries", err, calls)
	}
}

// docker start exits 0 for a container whose command dies at once; on the rig
// that read as revived and was revived again every scan.
func TestStayedUpConfirmsTheContainerIsStillRunning(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		st      containerState
		readErr error
		wantErr bool
	}{
		{"still running", containerState{Running: true}, nil, false},
		{"exited right after start", containerState{Running: false}, nil, true},
		{"state unreadable", containerState{}, errors.New("engine hiccup"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := stayedUp(ctx, time.Millisecond, func() (containerState, error) { return tc.st, tc.readErr })
			if (err != nil) != tc.wantErr || (err != nil && !errors.Is(err, rt.ErrWontStayUp)) {
				t.Errorf("err = %v, want rt.ErrWontStayUp %v", err, tc.wantErr)
			}
		})
	}
}
