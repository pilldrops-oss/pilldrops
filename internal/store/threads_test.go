package store

import (
	"strings"
	"testing"
	"time"
)

func TestThreadSurvivesAndIsOwnerScoped(t *testing.T) {
	st := openTemp(t)
	now := time.Now().Unix()

	th := Thread{ID: "th-1", OrgID: "acme", Owner: "u:1", Title: "checkout api", Turns: `[{"kind":"you"}]`, CreatedAt: now, UpdatedAt: now}
	if err := st.SaveThread(t.Context(), th); err != nil {
		t.Fatalf("SaveThread: %v", err)
	}

	got, err := st.GetThread(t.Context(), "acme", "th-1")
	if err != nil || got == nil {
		t.Fatalf("GetThread: %v %+v", err, got)
	}
	if got.Title != "checkout api" || got.Turns != `[{"kind":"you"}]` {
		t.Fatalf("round-trip lost data: %+v", got)
	}

	if list, _ := st.ListThreads(t.Context(), "other", 10); len(list) != 0 {
		t.Fatalf("thread leaked across orgs")
	}
	if other, _ := st.GetThread(t.Context(), "other", "th-1"); other != nil {
		t.Fatalf("GetThread reached another org's thread")
	}
}

func TestThreadListIsMostRecentlyUpdatedFirst(t *testing.T) {
	st := openTemp(t)
	now := time.Now().Unix()
	_ = st.SaveThread(t.Context(), Thread{ID: "old", OrgID: "acme", Title: "old", CreatedAt: now - 900, UpdatedAt: now - 900})
	_ = st.SaveThread(t.Context(), Thread{ID: "fresh", OrgID: "acme", Title: "fresh", CreatedAt: now - 500, UpdatedAt: now})

	list, _ := st.ListThreads(t.Context(), "acme", 10)
	if len(list) != 2 || list[0].ID != "fresh" {
		t.Fatalf("not ordered by recent activity: %+v", list)
	}
	if list[0].Turns != "" {
		t.Fatalf("list returned full transcripts: %q", list[0].Turns)
	}
}

func TestSaveThreadIsIdempotent(t *testing.T) {
	st := openTemp(t)
	now := time.Now().Unix()
	th := Thread{ID: "th-1", OrgID: "acme", Title: "first", Turns: `[1]`, CreatedAt: now, UpdatedAt: now}
	if err := st.SaveThread(t.Context(), th); err != nil {
		t.Fatalf("first: %v", err)
	}
	th.Title = "renamed"
	th.Turns = `[1,2]`
	th.UpdatedAt = now + 5
	if err := st.SaveThread(t.Context(), th); err != nil {
		t.Fatalf("second: %v", err)
	}
	list, _ := st.ListThreads(t.Context(), "acme", 10)
	if len(list) != 1 || list[0].Title != "renamed" {
		t.Fatalf("not upserted: %+v", list)
	}
	got, _ := st.GetThread(t.Context(), "acme", "th-1")
	if got.Turns != `[1,2]` {
		t.Fatalf("turns not updated: %q", got.Turns)
	}
}

func TestDeleteThreadRemovesIt(t *testing.T) {
	st := openTemp(t)
	now := time.Now().Unix()
	_ = st.SaveThread(t.Context(), Thread{ID: "th-1", OrgID: "acme", Title: "x", CreatedAt: now, UpdatedAt: now})
	if err := st.DeleteThread(t.Context(), "acme", "th-1"); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}
	if got, _ := st.GetThread(t.Context(), "acme", "th-1"); got != nil {
		t.Fatalf("thread still present after delete")
	}
	_ = st.SaveThread(t.Context(), Thread{ID: "th-2", OrgID: "acme", Title: "y", CreatedAt: now, UpdatedAt: now})
	_ = st.DeleteThread(t.Context(), "other", "th-2")
	if got, _ := st.GetThread(t.Context(), "acme", "th-2"); got == nil {
		t.Fatalf("another org deleted our thread")
	}
}

func TestThreadTranscriptIsCapped(t *testing.T) {
	st := openTemp(t)
	now := time.Now().Unix()
	huge := strings.Repeat("x", maxThreadBytes+5000)
	if err := st.SaveThread(t.Context(), Thread{ID: "th-1", OrgID: "acme", Title: "big", Turns: huge, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("SaveThread: %v", err)
	}
	got, _ := st.GetThread(t.Context(), "acme", "th-1")
	if len(got.Turns) > maxThreadBytes {
		t.Fatalf("stored %d bytes, cap is %d", len(got.Turns), maxThreadBytes)
	}
}
