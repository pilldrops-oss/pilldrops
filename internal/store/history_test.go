package store

import (
	"testing"
	"time"
)

func TestSandboxHistory(t *testing.T) {
	st := openTemp(t)
	now := time.Now().Unix()
	rec := HistoryRecord{
		ID: "sb-1", OrgID: "acme", Owner: "u:1", Template: "postgres", Image: "postgres:16",
		Node: "node-1", ExitState: "destroyed", CreatedAt: now - 120, DestroyedAt: now,
		DurationSecs: 120, VCPU: 2, MemMiB: 512,
		VCPUSeconds: 240, RAMGBSeconds: 60,
		Client: "cli",
		Logs:   "line1\nline2\n[destroyed]",
	}
	if err := st.RecordHistory(t.Context(), rec); err != nil {
		t.Fatalf("RecordHistory: %v", err)
	}

	list, err := st.ListHistory(t.Context(), "acme", 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListHistory: %v n=%d", err, len(list))
	}
	if list[0].ID != "sb-1" || list[0].Template != "postgres" || list[0].DurationSecs != 120 {
		t.Fatalf("list row wrong: %+v", list[0])
	}
	if list[0].Client != "cli" {
		t.Fatalf("list client = %q, want 'cli'", list[0].Client)
	}
	if list[0].VCPUSeconds != 240 || list[0].RAMGBSeconds != 60 {
		t.Fatalf("list usage fields wrong: vcpu=%v ram=%v",
			list[0].VCPUSeconds, list[0].RAMGBSeconds)
	}
	if list[0].Logs != "" {
		t.Fatalf("list should omit logs, got %q", list[0].Logs)
	}
	if other, _ := st.ListHistory(t.Context(), "other-org", 10); len(other) != 0 {
		t.Fatalf("cross-org leak: %d rows", len(other))
	}

	got, ok, err := st.GetHistory(t.Context(), "sb-1")
	if err != nil || !ok {
		t.Fatalf("GetHistory: %v ok=%v", err, ok)
	}
	if got.Logs == "" || got.Image != "postgres:16" || got.Owner != "u:1" {
		t.Fatalf("detail wrong: %+v", got)
	}
	if got.Client != "cli" {
		t.Fatalf("GetHistory client = %q, want 'cli'", got.Client)
	}

	n, err := st.PruneOlderThan(t.Context(), retentionTargetForTest(t, "history"), now+1)
	if err != nil || n != 1 {
		t.Fatalf("prune history: %v n=%d", err, n)
	}
	if list2, _ := st.ListHistory(t.Context(), "acme", 10); len(list2) != 0 {
		t.Fatalf("prune left %d rows", len(list2))
	}
}
