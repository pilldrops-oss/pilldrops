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

// A dockerd restart without live-restore stops every container at once.
func TestConfirmDeadIgnoresContainersADockerdRestartStopped(t *testing.T) {
	i := &dockerInstance{}
	t0 := time.Now()
	stopped, restarted := t0.Add(-time.Minute), t0.Add(-50*time.Second)
	for _, now := range []time.Time{t0, t0.Add(time.Minute), t0.Add(time.Hour)} {
		if i.confirmDead(now, stopped, restarted) {
			t.Fatalf("a container stopped by the dockerd restart was reported dead at %v", now)
		}
	}
	// One that stopped after the restart is still reaped.
	after := t0.Add(-10 * time.Second)
	i.confirmDead(t0, after, restarted)
	if !i.confirmDead(t0.Add(deadConfirmAfter), after, restarted) {
		t.Fatal("a container that died after the restart was not reported dead")
	}
}
