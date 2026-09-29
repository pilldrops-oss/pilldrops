package httpapi

import (
	"testing"
	"time"
)

func TestResolveExecTimeout(t *testing.T) {
	cases := []struct {
		name    string
		seconds int
		detach  bool
		want    time.Duration
	}{
		{"naive foreground gets default (not 30s)", 0, false, defaultForegroundExecTimeout},
		{"negative treated as unset", -1, false, defaultForegroundExecTimeout},
		{"client value honored", 45, false, 45 * time.Second},
		{"client value clamped to max", 100000, false, maxForegroundExecTimeout},
		{"detached passes through (unset)", 0, true, 0},
		{"detached passes through (set)", 10, true, 10 * time.Second},
	}
	for _, c := range cases {
		if got := resolveExecTimeout(c.seconds, c.detach); got != c.want {
			t.Errorf("%s: resolveExecTimeout(%d, %v) = %v, want %v", c.name, c.seconds, c.detach, got, c.want)
		}
	}
	if defaultForegroundExecTimeout <= 30*time.Second {
		t.Fatalf("default %v must exceed the 30s floor it replaces", defaultForegroundExecTimeout)
	}
}
