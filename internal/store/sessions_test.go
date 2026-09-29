package store

import "testing"

func TestAdminSessionsAndErrors(t *testing.T) {
	st := openTemp(t)
	now := int64(3_000_000)

	for _, e := range []struct {
		owner, email string
		at           int64
	}{
		{"u:abc", "a@b.com", now},
		{"u:abc", "a@b.com", now - 3600},
		{"u:xyz", "x@y.com", now - 5*86400},
	} {
		if err := st.RecordLoginEvent(t.Context(), e.at, e.owner, e.email); err != nil {
			t.Fatalf("RecordLoginEvent: %v", err)
		}
	}
	if err := st.RecordLoginEvent(t.Context(), now, "", ""); err != nil {
		t.Fatalf("RecordLoginEvent(empty): %v", err)
	}

	sess, err := st.AdminSessions(t.Context(), now, 30)
	if err != nil {
		t.Fatalf("AdminSessions: %v", err)
	}
	if sess.Total != 3 {
		t.Fatalf("Total=%d want 3", sess.Total)
	}
	if sess.Logins24h != 2 {
		t.Fatalf("Logins24h=%d want 2", sess.Logins24h)
	}
	if sess.UniqueUsers != 2 {
		t.Fatalf("UniqueUsers=%d want 2", sess.UniqueUsers)
	}
	if len(sess.ByDay) == 0 || len(sess.Recent) != 3 {
		t.Fatalf("ByDay=%d Recent=%d", len(sess.ByDay), len(sess.Recent))
	}
	if sess.Recent[0].At != now {
		t.Fatalf("Recent[0].At=%d want %d", sess.Recent[0].At, now)
	}

	if err := st.RecordHistory(t.Context(), HistoryRecord{ID: "ok1", OrgID: "u:abc", Template: "node", ExitState: "exited", DestroyedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordHistory(t.Context(), HistoryRecord{ID: "bad1", OrgID: "u:abc", Template: "node", ExitState: "crashed", DestroyedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordHistory(t.Context(), HistoryRecord{ID: "bad2", OrgID: "u:xyz", Template: "go", ExitState: "oom", DestroyedAt: now - 10}); err != nil {
		t.Fatal(err)
	}
	errs, err := st.AdminErrors(t.Context(), 100)
	if err != nil {
		t.Fatalf("AdminErrors: %v", err)
	}
	if len(errs) != 2 {
		t.Fatalf("AdminErrors=%d want 2 (crashed+oom)", len(errs))
	}
	if errs[0].ID != "bad1" {
		t.Fatalf("errs[0].ID=%s want bad1", errs[0].ID)
	}
}
