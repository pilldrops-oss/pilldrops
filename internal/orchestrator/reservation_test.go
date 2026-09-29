package orchestrator

import (
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

// Two concurrent creates must not both pick the same node before either is reflected in capacity.
func TestReservationPreventsDoubleSchedule(t *testing.T) {
	s := NewServer("k", "t")
	now := time.Now()
	// Room for exactly one 4000 MB sandbox given the 512 MB margin.
	upsert(s.reg, "node-a", "http://x", "kk", "", nil, "",
		cluster.Capacity{MemTotalMB: 8192, MemAvailMB: 5000, MemCommittedMB: 3000}, now)

	req := cluster.Request{MemMB: 4000}
	if first := s.reg.PickAndReserve(req, now, cluster.NodeTTL); first == nil || first.ID != "node-a" {
		t.Fatalf("first PickAndReserve = %v, want node-a", first)
	}
	if second := s.reg.PickAndReserve(req, now, cluster.NodeTTL); second != nil {
		t.Fatalf("second PickAndReserve = %v, want nil (reservation must block the double-schedule)", second)
	}

	s.reg.Release("node-a", 4000)
	if third := s.reg.PickAndReserve(req, now, cluster.NodeTTL); third == nil {
		t.Fatal("after Release, node-a should fit again")
	}
}
