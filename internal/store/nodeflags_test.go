package store

import (
	"context"
	"testing"
)

func TestNodeFlagsRoundTrip(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	for _, row := range []NodeFlagsRow{
		{NodeID: "a", Cordoned: true},
		{NodeID: "b", Cordoned: true},
		{NodeID: "a", Cordoned: true, Draining: true},
		{NodeID: "b"},
	} {
		if err := st.SaveNodeFlags(ctx, row); err != nil {
			t.Fatalf("save %+v: %v", row, err)
		}
	}
	rows, err := st.NodeFlags(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0] != (NodeFlagsRow{NodeID: "a", Cordoned: true, Draining: true}) {
		t.Errorf("rows = %+v, want only a, cordoned and draining", rows)
	}
}
