package nodeboot

import (
	"os"
	"regexp"
	"testing"
)

// FC_SNAPSHOTS (golden restore, read by nodeboot) and FC_MEM_SNAPSHOTS
// (pause/resume, read by the firecracker adapter) are two features. Pinned as
// source strings because nodeboot must not import the adapter.
func TestSnapshotFlagsAreTwoSeparateFeatures(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	flags := func(body, path string) map[string]bool {
		t.Helper()
		re := regexp.MustCompile(`"(FC_[A-Z_]*SNAPSHOT[A-Z_]*)"`)
		found := map[string]bool{}
		for _, m := range re.FindAllStringSubmatch(body, -1) {
			found[m[1]] = true
		}
		if len(found) == 0 {
			t.Fatalf("no FC_*SNAPSHOT* env name found in %s — the pattern has drifted and "+
				"this test is asserting over an empty set", path)
		}
		return found
	}

	const nodebootRuntime = "runtime.go"
	const adapter = "../runtime/firecracker/adapter.go"
	nb := flags(read(nodebootRuntime), nodebootRuntime)
	ad := flags(read(adapter), adapter)

	if !nb["FC_SNAPSHOTS"] {
		t.Errorf("%s no longer reads FC_SNAPSHOTS. That is the flag that turns golden "+
			"restore on (vm.Config.EnableSnapshots); if it moved, every operator runbook "+
			"and docs/deployment.md now name a variable nothing reads", nodebootRuntime)
	}
	if !ad["FC_MEM_SNAPSHOTS"] {
		t.Errorf("%s no longer reads FC_MEM_SNAPSHOTS — the flag that enables per-sandbox "+
			"pause/resume", adapter)
	}
	if nb["FC_MEM_SNAPSHOTS"] || ad["FC_SNAPSHOTS"] {
		t.Errorf("the two snapshot flags have been merged (nodeboot reads %v, the adapter "+
			"reads %v). That may well be right — they were one guard's business until "+
			"2026-09-03 — but it changes what an existing node does on an existing "+
			"setting: a node running FC_MEM_SNAPSHOTS=1 for pause/resume would start "+
			"building golden snapshots at boot, which costs a cold boot per template per "+
			"version and fails after 60s each if the template's baked envd predates the "+
			"vsock credential channel. Update docs/deployment.md and this test together",
			nb, ad)
	}
}
