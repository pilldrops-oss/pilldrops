package orchestrator

import (
	"net/http"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

// Two Firecracker nodes, only one of which can boot a published capture: on
// firecracker named_snapshot is FC_MEM_SNAPSHOTS and FC_NETNS together, so the
// runtime pin a snapshot create already carries promises neither. The node
// without the capability is the better fit and wins placement on fit alone,
// which is what made this a 501 the caller could do nothing about.
func TestCreateFromSnapshotIsPlacedOnANodeThatCanBootOne(t *testing.T) {
	f := newSnapshotCreateFleet(t)

	// The capable node is the one the fixture already registered, at 65536 MB
	// free. Give it the bit, and add a tighter-fitting Firecracker node without it.
	f.s.reg.SetNodeMeta("node-create-fc", cluster.NodeMeta{
		Runtime: "firecracker", Isolation: true, CanFetch: true, NamedSnapshot: true,
	})
	plain := newCreateRecorder(t, "fc-plain")
	if err := upsert(f.s.reg, "node-create-fc-plain", plain.srv.URL, "nodekey", "", nil, "v1",
		cluster.Capacity{Cores: 8, MemTotalMB: 65536, MemAvailMB: 9000}, time.Now()); err != nil {
		t.Fatal(err)
	}
	f.s.reg.SetNodeMeta("node-create-fc-plain", cluster.NodeMeta{
		Runtime: "firecracker", Isolation: true, CanFetch: true,
	})

	// Control: an ordinary create on the same runtime goes to the tighter fit.
	// Without it, "the snapshot create went to the capable node" would prove nothing.
	if status, out := f.do(t, "POST", "/v1/sandboxes", "apikey",
		`{"template":"base","runtime":"firecracker"}`); status != http.StatusCreated {
		t.Fatalf("plain create answered %d %v", status, out["_raw"])
	}
	plain.mu.Lock()
	plainGot := len(plain.got)
	plain.mu.Unlock()
	if plainGot != 1 {
		t.Fatalf("the control create did not land on the best-fit node (%d creates there); "+
			"the fixture no longer proves the filter is what moves the snapshot create", plainGot)
	}

	if got := f.saveAndWait(t, "golden"); got["status"] != "ready" {
		t.Fatalf("save: %v", got)
	}
	if status, out := f.do(t, "POST", "/v1/sandboxes", "apikey", `{"snapshot":"golden"}`); status != http.StatusCreated {
		t.Fatalf("create from a snapshot answered %d %v", status, out["_raw"])
	}

	plain.mu.Lock()
	after := len(plain.got)
	plain.mu.Unlock()
	if after != plainGot {
		t.Errorf("the snapshot create landed on a node that does not report named_snapshot; "+
			"it answers 501 there and the caller cannot pick a different node (%d creates)", after)
	}
	if spec := f.fc.last(t); spec.Snapshot != "golden" {
		t.Errorf("the capable node was asked for %q, want the snapshot create", spec.Snapshot)
	}
}
