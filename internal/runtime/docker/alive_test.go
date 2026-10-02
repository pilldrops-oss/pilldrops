package docker

import (
	"context"
	"errors"
	"testing"
	"time"
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

// A dockerd restart without live-restore stops every container at once, just
// before the new daemon starts (graceful) or just after (crash), and the
// daemon's start time is only known to the second.
func TestRestartCasualtyWindow(t *testing.T) {
	restarted := time.Now().Add(-10 * time.Minute)
	for _, tc := range []struct {
		name     string
		stopped  time.Time
		casualty bool
	}{
		{"long before the restart", restarted.Add(-restartWindowBefore - time.Second), false},
		{"at the window's early edge", restarted.Add(-restartWindowBefore), true},
		{"just before the restart (graceful)", restarted.Add(-time.Second), true},
		{"just after the restart (btime truncation)", restarted.Add(500 * time.Millisecond), true},
		{"killed by the new daemon (crash)", restarted.Add(20 * time.Second), true},
		{"at the window's late edge", restarted.Add(restartWindowAfter), true},
		{"died on its own after the restart", restarted.Add(restartWindowAfter + time.Second), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := restartCasualty(tc.stopped, restarted); got != tc.casualty {
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
