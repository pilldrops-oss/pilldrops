package store

import "testing"

func TestMigrationV11Waitlist(t *testing.T) {
	st := openTemp(t)
	if err := st.AddWaitlist(t.Context(), "a@b.com", "pricing", "", 100); err != nil {
		t.Fatalf("AddWaitlist: %v", err)
	}
	var name string
	if err := st.queryRow(t.Context(), `SELECT name FROM schema_migrations WHERE version = 11`).Scan(&name); err != nil || name != "waitlist" {
		t.Fatalf("v11 not recorded (name=%q err=%v)", name, err)
	}
}

// A repeat signup keeps the first created_at but takes the latest source and note.
func TestAddWaitlistIdempotent(t *testing.T) {
	st := openTemp(t)
	if err := st.AddWaitlist(t.Context(), "a@b.com", "pricing", "first note", 100); err != nil {
		t.Fatalf("first AddWaitlist: %v", err)
	}
	if err := st.AddWaitlist(t.Context(), "a@b.com", "billing", "second note", 200); err != nil {
		t.Fatalf("second AddWaitlist: %v", err)
	}

	rows, err := st.ListWaitlist(t.Context(), 10)
	if err != nil {
		t.Fatalf("ListWaitlist: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (idempotent upsert, no duplicate)", len(rows))
	}
	if rows[0].CreatedAt != 100 {
		t.Fatalf("created_at = %d, want 100 (the FIRST signup's time)", rows[0].CreatedAt)
	}
	if rows[0].Source != "billing" || rows[0].Note != "second note" {
		t.Fatalf("source/note = %q/%q, want the latest signup's values", rows[0].Source, rows[0].Note)
	}
}

func TestAddWaitlistEmailNormalization(t *testing.T) {
	st := openTemp(t)
	if err := st.AddWaitlist(t.Context(), "  A@B.com  ", "pricing", "", 100); err != nil {
		t.Fatalf("AddWaitlist: %v", err)
	}
	if err := st.AddWaitlist(t.Context(), "a@b.com", "billing", "", 200); err != nil {
		t.Fatalf("AddWaitlist (normalized dup): %v", err)
	}
	rows, err := st.ListWaitlist(t.Context(), 10)
	if err != nil {
		t.Fatalf("ListWaitlist: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (case/whitespace-insensitive email collision)", len(rows))
	}
	if rows[0].Email != "a@b.com" {
		t.Fatalf("email = %q, want normalized %q", rows[0].Email, "a@b.com")
	}
}

func TestAddWaitlistValidation(t *testing.T) {
	st := openTemp(t)
	cases := []string{"", "   ", "notanemail", "a@b.com" + string(make([]byte, 250))}
	for _, email := range cases {
		if err := st.AddWaitlist(t.Context(), email, "pricing", "", 100); err == nil {
			t.Errorf("AddWaitlist(%q) = nil error, want a validation error", email)
		}
	}
}

func TestListWaitlistOrderingAndClamp(t *testing.T) {
	st := openTemp(t)
	for i, email := range []string{"a@x.com", "b@x.com", "c@x.com"} {
		if err := st.AddWaitlist(t.Context(), email, "pricing", "", int64(100+i*10)); err != nil {
			t.Fatalf("AddWaitlist(%s): %v", email, err)
		}
	}
	rows, err := st.ListWaitlist(t.Context(), 10)
	if err != nil {
		t.Fatalf("ListWaitlist: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	if rows[0].Email != "c@x.com" || rows[1].Email != "b@x.com" || rows[2].Email != "a@x.com" {
		t.Fatalf("order = %v, want newest-first [c,b,a]", rows)
	}

	rows, err = st.ListWaitlist(t.Context(), 0)
	if err != nil || len(rows) != 3 {
		t.Fatalf("ListWaitlist(0) = %d rows, err=%v — want 3 rows (clamped to default 200)", len(rows), err)
	}

	rows, err = st.ListWaitlist(t.Context(), 5000)
	if err != nil || len(rows) != 3 {
		t.Fatalf("ListWaitlist(5000) = %d rows, err=%v — want 3 rows (clamped to 1000)", len(rows), err)
	}
}
