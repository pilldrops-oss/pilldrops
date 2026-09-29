package daemon

import "testing"

func TestDiscriminatesByPortDetectsANonDiscriminatingRuntime(t *testing.T) {
	// Stand-in for docker/process: the same address whatever is asked.
	sameAlways := func(int) (string, bool) { return "127.0.0.1:9", true }
	if discriminatesByPort(sameAlways) {
		t.Fatal("a runtime that returns one address for every port must NOT be trusted")
	}
}

func TestDiscriminatesByPortAcceptsAPortAwareRuntime(t *testing.T) {
	// Stand-in for Firecracker: only the real app port answers.
	portAware := func(p int) (string, bool) { return "10.0.0.2:8000", p == 8000 }
	if !discriminatesByPort(portAware) {
		t.Fatal("a runtime that answers per-port must be trusted")
	}
}

// Nothing reachable is still a discriminating runtime, just one with nothing running.
func TestDiscriminatesByPortWithNothingListening(t *testing.T) {
	nothing := func(int) (string, bool) { return "", false }
	if !discriminatesByPort(nothing) {
		t.Fatal("nothing listening is a valid answer, not a broken runtime")
	}
}
