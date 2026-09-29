package httpapi

import (
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/httpx"
)

// TestForegroundExecCeilingIsWhatTheHopCanCarry keeps the node's own ceiling and
// the control plane's first-byte bound from drifting apart. They did: the node
// admitted a 600-second foreground exec while the hop gave up at 30, so every
// command between the two completed here and 502'd there with its result lost.
//
// Either constant moving alone is the bug, so the check is the relationship,
// not the numbers.
func TestForegroundExecCeilingIsWhatTheHopCanCarry(t *testing.T) {
	if maxForegroundExecTimeout != httpx.NodeSyncCeiling {
		t.Fatalf("a foreground exec may run for %s but the control plane sizes its node hop from "+
			"%s — anything between the two is work this node finishes and the caller never sees",
			maxForegroundExecTimeout, httpx.NodeSyncCeiling)
	}
	if got := httpx.NewTransport().ResponseHeaderTimeout; got <= maxForegroundExecTimeout {
		t.Fatalf("the hop's first-byte bound (%s) does not outlast a foreground exec (%s)",
			got, maxForegroundExecTimeout)
	}
	// The create path's own wait has to fit inside the same budget, or a tenant
	// gets a transport timeout instead of "the image is still downloading".
	if templateFetchWait >= maxForegroundExecTimeout {
		t.Fatalf("templateFetchWait %s is not a short answer", templateFetchWait)
	}
}
