package cluster

import (
	"testing"
	"time"
)

// v2 and v3 are the two fleet generations, in the shape the heartbeat carries.
var (
	v2 = []string{"avx", "cx16", "popcnt", "sse4_1", "sse4_2", "ssse3"}
	v3 = []string{"avx", "avx2", "bmi1", "bmi2", "cx16", "fma", "popcnt", "sse4_1", "sse4_2", "ssse3"}
)

func cpuNode(id string, feats []string) *Node {
	return &Node{ID: id, Runtime: "firecracker", NamedSnapshot: true, CPUFeatures: feats,
		LastSeen: time.Now(), Cap: Capacity{MemTotalMB: 31000, MemAvailMB: 30000}}
}

// TestASnapshotIsNotPlacedOnACPUThatCannotRunIt is the placement half of the
// SIGILL. Both nodes fit, both can boot a named snapshot, and the poorer one is
// the better fit by RAM -- so anything that ignores the CPU picks it, which is
// exactly what the fleet did.
func TestASnapshotIsNotPlacedOnACPUThatCannotRunIt(t *testing.T) {
	now := time.Now()
	poor := cpuNode("ivybridge", v2)
	poor.Cap.MemAvailMB = 20000 // the better fit, so only the CPU can exclude it
	rich := cpuNode("haswell", v3)
	nodes := []*Node{poor, rich}

	got := Pick(nodes, Request{VCPU: 1, MemMB: 1024, NamedSnapshot: true, CPUFeatures: v3}, now, NodeTTL)
	if got == nil {
		t.Fatal("a snapshot that one online node can run was not placed at all")
	}
	if got.ID != rich.ID {
		t.Fatalf("a v3 snapshot was placed on %s, whose CPU lacks avx2/bmi1/bmi2/fma — "+
			"the guest chose those code paths before it was frozen and dies with SIGILL", got.ID)
	}

	// The other direction stays placeable on either, and the tighter fit wins.
	got = Pick(nodes, Request{VCPU: 1, MemMB: 1024, NamedSnapshot: true, CPUFeatures: v2}, now, NodeTTL)
	if got == nil || got.ID != poor.ID {
		t.Fatalf("a v2 snapshot did not take the best fit (%v) — poorer-to-richer is safe, "+
			"so neither node should be excluded", got)
	}

	// Nothing fits: the caller must be refused rather than given the poor node.
	if got := Pick(nodes, Request{VCPU: 1, MemMB: 1024, CPUFeatures: []string{"avx512f"}}, now, NodeTTL); got != nil {
		t.Fatalf("a snapshot needing avx512f was placed on %s", got.ID)
	}
}

// TestANodeThatReportsNoFeaturesIsStillPlaceable is the rolling-upgrade
// allowance: a node whose build predates the heartbeat field sends nothing, and
// reading that as "a CPU with no features" would refuse every snapshot create
// for the length of the roll. The node's own restore check is what keeps this
// from being a silent SIGILL.
func TestANodeThatReportsNoFeaturesIsStillPlaceable(t *testing.T) {
	old := cpuNode("pre-upgrade", nil)
	got := Pick([]*Node{old}, Request{VCPU: 1, MemMB: 1024, NamedSnapshot: true, CPUFeatures: v3},
		time.Now(), NodeTTL)
	if got == nil {
		t.Fatal("a node that does not report its CPU was excluded; a rolling upgrade would " +
			"refuse every create from a snapshot")
	}
}
