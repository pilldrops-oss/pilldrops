package store

import (
	"context"
	"testing"
)

func TestNodeRemovedRoundTrips(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	removed, err := s.NodeRemoved(ctx, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if removed {
		t.Fatal("a node nothing ever removed reports removed")
	}

	if err := s.MarkNodeRemoved(ctx, "node-1", "par1", 1000); err != nil {
		t.Fatal(err)
	}
	removed, err = s.NodeRemoved(ctx, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("MarkNodeRemoved did not stick")
	}
}

func TestMarkNodeRemovedIsIdempotent(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	// Removing an offline node, then again once it is seen again, must update
	// rather than error — the operator may retry, and a node can be removed
	// more than once across restarts.
	if err := s.MarkNodeRemoved(ctx, "node-1", "par1", 1000); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkNodeRemoved(ctx, "node-1", "par1", 2000); err != nil {
		t.Fatalf("re-removing an already-removed node errored: %v", err)
	}
	ids, err := s.RemovedNodeIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "node-1" {
		t.Fatalf("RemovedNodeIDs = %v, want exactly [node-1]", ids)
	}
}

func TestRemovedNodeIDsListsEveryRemoval(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	for _, id := range []string{"node-a", "node-b", "node-c"} {
		if err := s.MarkNodeRemoved(ctx, id, "par1", 1000); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := s.RemovedNodeIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 {
		t.Fatalf("RemovedNodeIDs = %v, want 3 entries", ids)
	}
}
