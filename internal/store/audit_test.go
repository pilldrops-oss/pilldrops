package store

import (
	"testing"
)

func TestAuditRoundTrip(t *testing.T) {
	st := openTemp(t)

	if err := st.RecordAudit(t.Context(), "org1", "key_1", "sandbox.create", "sb_abc"); err != nil {
		t.Fatalf("record org1: %v", err)
	}
	if err := st.RecordAudit(t.Context(), "org2", "key_2", "sandbox.create", "sb_xyz"); err != nil {
		t.Fatalf("record org2: %v", err)
	}

	entries, err := st.ListAudit(t.Context(), "org1", 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("org1 audit len = %d, want 1 (must be org-scoped)", len(entries))
	}
	e := entries[0]
	if e.Action != "sandbox.create" || e.Target != "sb_abc" || e.KeyID != "key_1" || e.OrgID != "org1" {
		t.Fatalf("unexpected entry: %+v", e)
	}
	if e.At == 0 {
		t.Errorf("audit entry has no timestamp")
	}
}

func TestAuditDetailRoundTrip(t *testing.T) {
	st := openTemp(t)
	if err := st.RecordAuditDetail(t.Context(), "org-a", "k1", "sandbox.exec", "sb-1", `{"cmd":"ls","exit_code":0}`); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordAuditDetail(t.Context(), "org-b", "k2", "sandbox.fs.write", "sb-1", `{"path":"/a.py","size":10}`); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordAudit(t.Context(), "org-a", "k1", "sandbox.destroy", "sb-2"); err != nil {
		t.Fatal(err)
	}

	got, err := st.ListAuditByTarget(t.Context(), "sb-1", 0)
	if err != nil || len(got) != 2 {
		t.Fatalf("by-target: err=%v n=%d", err, len(got))
	}
	if got[0].Action != "sandbox.exec" || got[0].Detail != `{"cmd":"ls","exit_code":0}` {
		t.Fatalf("row0 = %+v", got[0])
	}
	if got[1].OrgID != "org-b" || got[1].Detail != `{"path":"/a.py","size":10}` {
		t.Fatalf("row1 = %+v", got[1])
	}

	if rows, err := st.ListAuditByTarget(t.Context(), "sb-2", 0); err != nil || len(rows) != 1 || rows[0].Detail != "" {
		t.Fatalf("sb-2 rows = %+v err=%v", rows, err)
	}

	entries, err := st.ListAudit(t.Context(), "org-a", 0)
	if err != nil || len(entries) == 0 {
		t.Fatalf("ListAudit: %v n=%d", err, len(entries))
	}
	for _, e := range entries {
		if e.Detail != "" {
			t.Fatalf("ListAudit leaked detail: %+v", e)
		}
	}
}

func TestListAuditByTargetKeepsNewestUnderLimit(t *testing.T) {
	st := openTemp(t)
	for i := 0; i < 7; i++ {
		action := "a" + string(rune('0'+i))
		if err := st.RecordAuditDetail(t.Context(), "org-a", "k1", action, "sb-1", "{}"); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.ListAuditByTarget(t.Context(), "sb-1", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("len = %d, want 5", len(got))
	}
	want := []string{"a2", "a3", "a4", "a5", "a6"}
	for i, w := range want {
		if got[i].Action != w {
			t.Fatalf("row %d action = %q, want %q (full: %+v)", i, got[i].Action, w, got)
		}
	}
}
