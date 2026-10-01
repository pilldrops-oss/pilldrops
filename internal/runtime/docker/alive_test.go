package docker

import (
	"testing"
	"time"
)

func TestConfirmDeadNeedsTwoReadsApart(t *testing.T) {
	i := &dockerInstance{}
	t0 := time.Now()
	stopped := t0.Add(-time.Second)
	if i.confirmDead(t0, stopped, time.Time{}) {
		t.Fatal("one stopped read reaped the sandbox")
	}
	if i.confirmDead(t0.Add(time.Second), stopped, time.Time{}) {
		t.Fatal("a second read inside the confirmation window reaped it")
	}
	if !i.confirmDead(t0.Add(deadConfirmAfter), stopped, time.Time{}) {
		t.Fatal("a container stopped across two reads was not reported dead")
	}
}

// A dockerd restart without live-restore stops every container at once, just
// before the new daemon starts (graceful) or just after (crash), and the
// daemon's start time is only known to the second.
func TestConfirmDeadIgnoresContainersADockerdRestartStopped(t *testing.T) {
	restarted := time.Now().Add(-10 * time.Minute)
	for _, tc := range []struct {
		name    string
		stopped time.Time
		reaped  bool
	}{
		{"long before the restart", restarted.Add(-restartWindowBefore - time.Second), true},
		{"at the window's early edge", restarted.Add(-restartWindowBefore), false},
		{"just before the restart (graceful)", restarted.Add(-time.Second), false},
		{"just after the restart (btime truncation)", restarted.Add(500 * time.Millisecond), false},
		{"killed by the new daemon (crash)", restarted.Add(20 * time.Second), false},
		{"at the window's late edge", restarted.Add(restartWindowAfter), false},
		{"died on its own after the restart", restarted.Add(restartWindowAfter + time.Second), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := &dockerInstance{}
			now := time.Now()
			i.confirmDead(now, tc.stopped, restarted)
			if got := i.confirmDead(now.Add(deadConfirmAfter), tc.stopped, restarted); got != tc.reaped {
				t.Errorf("reaped = %v, want %v", got, tc.reaped)
			}
		})
	}
}
