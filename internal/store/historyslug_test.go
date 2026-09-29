package store

import (
	"testing"
)

// The help says a slug works wherever an id does. It did not here: `history
// <slug>` answered 404, and the live-sandbox slug resolver cannot help, because
// a history record exists exactly when that row is gone.
//
// Against the old code this fails on the lookup: the column did not exist and
// GetHistory matched on id alone.
func TestHistoryAnswersToTheSlugTheSandboxAnsweredTo(t *testing.T) {
	st := openTestStore(t)
	ctx := t.Context()

	if err := st.SaveSandbox(ctx, SandboxRecord{
		ID: "sb-1", OrgID: "org1", Slug: "my-api", Template: "base", State: "running",
	}); err != nil {
		t.Fatalf("put sandbox: %v", err)
	}
	if err := st.RecordHistory(ctx, HistoryRecord{
		ID: "sb-1", OrgID: "org1", Template: "base", ExitState: "destroyed",
		CreatedAt: 100, DestroyedAt: 200,
	}); err != nil {
		t.Fatalf("record history: %v", err)
	}

	rec, ok, err := st.GetHistory(ctx, "my-api")
	if err != nil {
		t.Fatalf("get by slug: %v", err)
	}
	if !ok {
		t.Fatal("history for a destroyed sandbox is not reachable by its slug")
	}
	if rec.ID != "sb-1" {
		t.Errorf("slug resolved to %q, want sb-1", rec.ID)
	}
	if rec.Slug != "my-api" {
		t.Errorf("record does not carry its slug back (%q)", rec.Slug)
	}

	if rec, ok, err := st.GetHistory(ctx, "sb-1"); err != nil || !ok || rec.ID != "sb-1" {
		t.Errorf("lookup by id: %+v ok=%v err=%v", rec, ok, err)
	}
	if _, ok, err := st.GetHistory(ctx, "never-existed"); err != nil || ok {
		t.Errorf("an unknown name resolved: ok=%v err=%v", ok, err)
	}
}

// A slug is free again once its sandbox is destroyed, so several records can
// carry it. The newest is the one the caller just lost.
func TestAReusedSlugResolvesToTheNewestRecord(t *testing.T) {
	st := openTestStore(t)
	ctx := t.Context()
	for _, c := range []struct {
		id        string
		destroyed int64
	}{{"sb-old", 100}, {"sb-new", 900}} {
		if err := st.SaveSandbox(ctx, SandboxRecord{ID: c.id, OrgID: "org1", Slug: "recycled", State: "running"}); err != nil {
			t.Fatal(err)
		}
		if err := st.RecordHistory(ctx, HistoryRecord{
			ID: c.id, OrgID: "org1", DestroyedAt: c.destroyed,
		}); err != nil {
			t.Fatal(err)
		}
		if err := st.DeleteSandbox(ctx, c.id); err != nil {
			t.Fatal(err)
		}
	}
	rec, ok, err := st.GetHistory(ctx, "recycled")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if rec.ID != "sb-new" {
		t.Errorf("a reused slug resolved to %q, want the most recently destroyed sb-new", rec.ID)
	}
}
