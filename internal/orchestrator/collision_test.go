package orchestrator

import (
	"errors"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

func TestUpsertRefusesTakeoverAndAllowsAGenuineMove(t *testing.T) {
	r := cluster.NewRegistry()
	t0 := time.Unix(1_700_000_000, 0)

	upsert(r, "node-x", "http://a:18080", "k", "par1", nil, "", cluster.Capacity{}, t0)
	if n, _ := r.Node("node-x"); n.URLChanges != 0 {
		t.Fatalf("first register URLChanges = %d, want 0", n.URLChanges)
	}

	err := upsert(r, "node-x", "http://b:18080", "k", "par1", nil, "", cluster.Capacity{}, t0.Add(3*time.Second))
	if !errors.Is(err, cluster.ErrNodeURLTakeover) {
		t.Fatalf("url change on a live node: err = %v, want ErrNodeURLTakeover", err)
	}
	n, _ := r.Node("node-x")
	if n.URL != "http://a:18080" || n.URLChanges != 0 {
		t.Fatalf("after a refused takeover url=%q URLChanges=%d, want http://a:18080 / 0", n.URL, n.URLChanges)
	}

	if err := upsert(r, "node-x", "http://a:18080", "k", "par1", nil, "", cluster.Capacity{}, t0.Add(6*time.Second)); err != nil {
		t.Fatalf("same-url heartbeat = %v, want nil", err)
	}
	if n, _ := r.Node("node-x"); n.URLChanges != 0 {
		t.Fatalf("same-url heartbeat bumped URLChanges to %d, want 0", n.URLChanges)
	}

	// Accepted and counted once the previous registration has aged past NodeTTL.
	moved := t0.Add(6*time.Second + cluster.NodeTTL + time.Second)
	if err := upsert(r, "node-x", "http://b:18080", "k", "par1", nil, "", cluster.Capacity{}, moved); err != nil {
		t.Fatalf("url change after the record went stale = %v, want nil", err)
	}
	n, _ = r.Node("node-x")
	if n.URL != "http://b:18080" || n.URLChanges != 1 {
		t.Fatalf("after a legitimate move url=%q URLChanges=%d, want http://b:18080 / 1", n.URL, n.URLChanges)
	}
	if !n.LastURLChangeAt.Equal(moved) {
		t.Fatalf("LastURLChangeAt = %v, want %v", n.LastURLChangeAt, moved)
	}
}

// Only active flapping is flagged; a settled one-time IP change is not.
func TestNodeViewFlagsURLConflict(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	flapping := &cluster.Node{ID: "n", URL: "http://b", URLChanges: 5, LastURLChangeAt: now, LastSeen: now}
	v := nodeView(flapping, now)
	if v["url_changes"] != 5 {
		t.Fatalf("url_changes = %v, want 5", v["url_changes"])
	}
	if v["url_conflict"] != true {
		t.Fatalf("actively flapping node url_conflict = %v, want true", v["url_conflict"])
	}

	onetime := &cluster.Node{ID: "n", URL: "http://b", URLChanges: 1, LastURLChangeAt: now, LastSeen: now}
	if nodeView(onetime, now)["url_conflict"] != false {
		t.Fatalf("single url change should not flag a conflict")
	}

	settled := &cluster.Node{ID: "n", URL: "http://b", URLChanges: 5, LastURLChangeAt: now.Add(-5 * time.Minute), LastSeen: now}
	if nodeView(settled, now)["url_conflict"] != false {
		t.Fatalf("settled node should not stay flagged after flapping stops")
	}
}
