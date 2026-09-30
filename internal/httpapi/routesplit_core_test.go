//go:build core

package httpapi

import (
	"strings"
	"testing"
)

// TestCoreBuildMountsOnlyTheCore checks the mux this build actually mounts,
// since registeredRoutes reads source text and ignores build tags.
// 81: the original 67 (47 in server.go + 5 image + 14 ops + /preview/) plus
// the backup, checkpoint, snapshot-export, fork and standby routes added since,
// filesystem/watch (82), and rehome, which the control plane calls on a
// fleet node to resume a paused sandbox whose own node is gone (83).
// It went stale while backupschedule_test.go kept the core build from compiling.
func TestCoreBuildMountsOnlyTheCore(t *testing.T) {
	const wantMounted = 83
	got := mountedRoutes(t)
	if len(got) == 0 {
		t.Fatal("the core build mounts nothing at all — either Register is empty or " +
			"the pattern-resolution helper has drifted, and this guard is passing on " +
			"an empty set")
	}
	if len(got) != wantMounted {
		t.Errorf("the core build mounts %d routes, expected %d.\n\n"+
			"If this GREW, a feature registrar lost its //go:build core no-op sibling "+
			"or a feature route was added to Register.\nIf it SHRANK, a core route was "+
			"moved behind the tag and a core node now 404s it.\nmounted: %v",
			len(got), wantMounted, got)
	}
}

// TestCoreBuildServesNoFeatureRoute checks no compiled-out feature endpoint
// answers, which the link-closure guard cannot see.
func TestCoreBuildServesNoFeatureRoute(t *testing.T) {
	forbidden := []string{"/agent", "/cron", "/functions", "/integrations", "/chat"}
	mounted := mountedRoutes(t)
	if len(mounted) == 0 {
		t.Fatal("nothing is mounted at all — this guard would pass on an empty set")
	}
	for _, r := range mounted {
		for _, f := range forbidden {
			if strings.Contains(r, f) {
				t.Errorf("the core build mounts %q, which belongs to a feature area that "+
					"is supposed to be compiled out. Its registrar's //go:build core "+
					"sibling is missing or does not cover it", r)
			}
		}
	}
}
