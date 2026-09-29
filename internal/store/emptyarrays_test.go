package store

import (
	"encoding/json"
	"testing"
)

// TestAdminReadsMarshalEmptyArraysNotNull asserts on the marshalled bytes,
// since a pre-seeded []T{} can still be overwritten by a nil result.
func TestAdminReadsMarshalEmptyArraysNotNull(t *testing.T) {
	st := openTemp(t)
	ctx := t.Context()

	fieldIsArray := func(what string, v any, fields ...string) {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s: marshal: %v", what, err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%s: not a JSON object: %v", what, err)
		}
		for _, f := range fields {
			raw, ok := m[f]
			if !ok {
				t.Fatalf("%s: no field %q in %s", what, f, b)
			}
			if string(raw) == "null" {
				t.Errorf("%s.%s = null, want [] — a consumer doing .length on it throws", what, f)
			}
			if len(raw) == 0 || raw[0] != '[' {
				t.Errorf("%s.%s = %s, want a JSON array", what, f, raw)
			}
		}
	}

	ov, err := st.AdminOverview(ctx, 0, 10)
	if err != nil {
		t.Fatalf("AdminOverview: %v", err)
	}
	fieldIsArray("overview", ov, "recent_errors")

	se, err := st.AdminSessions(ctx, 0, 7)
	if err != nil {
		t.Fatalf("AdminSessions: %v", err)
	}
	fieldIsArray("sessions", se, "recent")

	// These two are returned as bare slices and wrapped by the handler.
	sliceIsArray := func(what string, v any) {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s: marshal: %v", what, err)
		}
		if string(b) == "null" {
			t.Errorf("%s marshals to null, want []", what)
		}
	}

	users, err := st.ListUsers(ctx, 50)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	sliceIsArray("users", users)

	wl, err := st.ListWaitlist(ctx, 50)
	if err != nil {
		t.Fatalf("ListWaitlist: %v", err)
	}
	sliceIsArray("waitlist", wl)
}
