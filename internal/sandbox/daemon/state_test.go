package daemon

import "testing"

func TestTransitionGuards(t *testing.T) {
	cases := []struct {
		from  State
		event string
		ok    bool
	}{
		{StateRunning, "pause", true},
		{StatePaused, "pause", false},
		{StatePaused, "resume", true},
		{StateRunning, "resume", false},
		{StateRunning, "exec", true},
		{StatePaused, "exec", false},
		{StateStopped, "exec", false},
		{StateRunning, "destroy", true},
		{StatePaused, "destroy", true},
	}
	for _, c := range cases {
		if got := canTransition(c.from, c.event); got != c.ok {
			t.Errorf("canTransition(%s,%s) = %v, want %v", c.from, c.event, got, c.ok)
		}
	}
}
