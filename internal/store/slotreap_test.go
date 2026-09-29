package store

import (
	"context"
	"path/filepath"
	"testing"
)

func slotCount(t *testing.T, st *Store) int {
	t.Helper()
	var n int
	if err := st.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM admission_slots`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestExpiredSlotsAreReapedOnTheNextReservation(t *testing.T) {
	st, err := Open(t.Context(), filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	old := nowUnix() - slotWindowSecs - 60
	for _, id := range []string{"slot_a", "slot_b", "slot_c"} {
		if _, err := st.exec(t.Context(),
			`INSERT INTO admission_slots (id, org_id, created_at) VALUES (?, ?, ?)`,
			id, "u:acme", old); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if got := slotCount(t, st); got != 3 {
		t.Fatalf("seeded %d rows, want 3", got)
	}

	ok, err := st.ReserveSlot(t.Context(), "u:acme", "slot_new", 5)
	if err != nil {
		t.Fatalf("ReserveSlot: %v", err)
	}
	if !ok {
		t.Fatal("reservation refused — the stale rows are still being counted")
	}
	if got := slotCount(t, st); got != 1 {
		t.Errorf("%d rows left, want 1 (only the new claim)", got)
	}
}

// A quiet org never calls ReserveSlot again, so its rows need the fleet-wide sweep.
func TestPurgeExpiredSlotsCleansQuietOrgs(t *testing.T) {
	st, err := Open(t.Context(), filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	old := nowUnix() - slotWindowSecs - 60
	for _, org := range []string{"u:gone-a", "u:gone-b"} {
		if _, err := st.exec(t.Context(),
			`INSERT INTO admission_slots (id, org_id, created_at) VALUES (?, ?, ?)`,
			"slot_"+org, org, old); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// A live claim must survive the purge.
	if _, err := st.exec(t.Context(),
		`INSERT INTO admission_slots (id, org_id, created_at) VALUES (?, ?, ?)`,
		"slot_live", "u:active", nowUnix()); err != nil {
		t.Fatalf("seed live: %v", err)
	}

	n, err := st.PurgeExpiredSlots(t.Context())
	if err != nil {
		t.Fatalf("PurgeExpiredSlots: %v", err)
	}
	if n != 2 {
		t.Errorf("purged %d, want 2", n)
	}
	if got := slotCount(t, st); got != 1 {
		t.Errorf("%d rows left, want 1 — the live claim must survive", got)
	}
}
