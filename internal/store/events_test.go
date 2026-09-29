package store

import "testing"

func TestSandboxEvents(t *testing.T) {
	st := openTemp(t)

	// acme: create (1), create (2), pause (1), then much later a create (2).
	evs := []EventRecord{
		{OrgID: "acme", SandboxID: "a", Event: "created", Template: "base", ActiveAfter: 1, At: 1000},
		{OrgID: "acme", SandboxID: "b", Event: "created", Template: "base", ActiveAfter: 2, At: 1010},
		{OrgID: "acme", SandboxID: "a", Event: "paused", ActiveAfter: 1, At: 1020},
		{OrgID: "acme", SandboxID: "c", Event: "created", Template: "claude", ActiveAfter: 2, At: 5000},
		{OrgID: "other", SandboxID: "z", Event: "created", ActiveAfter: 1, At: 1015},
	}
	for _, e := range evs {
		if err := st.RecordEvent(t.Context(), e); err != nil {
			t.Fatalf("RecordEvent: %v", err)
		}
	}

	list, err := st.ListEvents(t.Context(), "acme", 10)
	if err != nil || len(list) != 4 {
		t.Fatalf("ListEvents: %v n=%d", err, len(list))
	}
	if list[0].SandboxID != "c" || list[0].Event != "created" || list[0].ActiveAfter != 2 {
		t.Fatalf("newest event wrong: %+v", list[0])
	}
	if other, _ := st.ListEvents(t.Context(), "other", 10); len(other) != 1 {
		t.Fatalf("cross-org leak: %d", len(other))
	}

	// Anchor (level 1, clamped to 4000) plus the create at 5000 (level 2).
	series, err := st.ConcurrencySeries(t.Context(), "acme", 4000)
	if err != nil {
		t.Fatalf("ConcurrencySeries: %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("series len = %d, want 2 (anchor + one event): %+v", len(series), series)
	}
	if series[0].At != 4000 || series[0].ActiveAfter != 1 {
		t.Errorf("anchor = (at %d, n %d), want (4000, 1)", series[0].At, series[0].ActiveAfter)
	}
	if series[1].At != 5000 || series[1].ActiveAfter != 2 {
		t.Errorf("point = (at %d, n %d), want (5000, 2)", series[1].At, series[1].ActiveAfter)
	}

	// Prune drops the four early events across both orgs.
	n, err := st.PruneOlderThan(t.Context(), retentionTargetForTest(t, "events"), 2000)
	if err != nil || n != 4 {
		t.Fatalf("prune events: %v n=%d (want 4)", err, n)
	}
}
