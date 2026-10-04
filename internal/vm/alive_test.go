//go:build linux

package vm

import (
	"os"
	"path/filepath"
	"testing"
)

// The SDK reports pid 0 once the VMM it launched has exited; that read as
// alive, so an OOM-killed VM stayed "running".
func TestAliveIsFalseOnceTheVMMHasNoPid(t *testing.T) {
	if newParkManager(t, 0).Alive("sb-park") {
		t.Error("a VMM with no pid reported alive")
	}
	if !newParkManager(t, os.Getpid()).Alive("sb-park") {
		t.Error("a running VMM reported dead")
	}
}

func TestVMMOOMKilledReadsTheLeafMemoryEvents(t *testing.T) {
	old := cgroupRoot
	cgroupRoot = t.TempDir()
	t.Cleanup(func() { cgroupRoot = old })

	write := func(id, events string) {
		dir := vmmCgroupPath(id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "memory.events"), []byte(events), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("sb-oom", "low 0\nhigh 0\nmax 12\noom 1\noom_kill 1\noom_group_kill 0\n")
	write("sb-ok", "low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\noom_group_kill 0\n")

	if !vmmOOMKilled("sb-oom") {
		t.Error("an oom_kill in the leaf was not reported")
	}
	if vmmOOMKilled("sb-ok") || vmmOOMKilled("sb-gone") {
		t.Error("an OOM was reported for a VMM with no oom_kill")
	}
}
