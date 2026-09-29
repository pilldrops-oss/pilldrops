package store

import (
	"testing"
	"unicode/utf8"
)

// source is inherited by history on destroy and surfaces in AdminOverview.
func TestSandboxSourceAttribution(t *testing.T) {
	st := openTemp(t)
	now := int64(4_000_000)

	mkSandbox(t, st, "sa1", "u:a", now)
	if err := st.SetSandboxAttribution(t.Context(), "sa1", "agent", "cli"); err != nil {
		t.Fatalf("SetSandboxAttribution: %v", err)
	}
	if err := st.RecordHistory(t.Context(), HistoryRecord{
		ID: "sa1", OrgID: "u:a", Template: "node", ExitState: "destroyed", CreatedAt: now, DestroyedAt: now + 10,
	}); err != nil {
		t.Fatalf("RecordHistory: %v", err)
	}
	if err := st.DeleteSandbox(t.Context(), "sa1"); err != nil {
		t.Fatalf("DeleteSandbox: %v", err)
	}
	recs, err := st.ListHistory(t.Context(), "u:a", 10)
	if err != nil {
		t.Fatalf("ListHistory: %v", err)
	}
	if len(recs) != 1 || recs[0].Source != "agent" {
		t.Fatalf("history source: recs=%d source=%q want 1 × agent", len(recs), historySource(recs))
	}

	mkSandbox(t, st, "sa2", "u:a", now)
	if err := st.SetSandboxAttribution(t.Context(), "sa2", "human", "web"); err != nil {
		t.Fatal(err)
	}

	o, err := st.AdminOverview(t.Context(), now, 14)
	if err != nil {
		t.Fatalf("AdminOverview: %v", err)
	}
	got := map[string]int64{}
	for _, ns := range o.BySource {
		got[ns.Name] = ns.Count
	}
	if got["agent"] != 1 || got["human"] != 1 {
		t.Fatalf("by_source = %+v want agent:1 human:1", o.BySource)
	}
}

func TestSetSandboxAttribution(t *testing.T) {
	st := openTemp(t)
	now := int64(5_000_000)
	mkSandbox(t, st, "sb1", "u:b", now)

	if err := st.SetSandboxAttribution(t.Context(), "sb1", "agent", "  CLI  "); err != nil {
		t.Fatalf("SetSandboxAttribution: %v", err)
	}
	row, ok, err := st.GetActiveSandboxRow(t.Context(), "sb1")
	if err != nil || !ok {
		t.Fatalf("GetActiveSandboxRow: ok=%v err=%v", ok, err)
	}
	if row.Source != "agent" {
		t.Fatalf("source = %q, want agent", row.Source)
	}
	var client string
	if err := st.queryRow(t.Context(), `SELECT client FROM sandboxes WHERE id = ?`, "sb1").Scan(&client); err != nil {
		t.Fatalf("read client: %v", err)
	}
	if client != "cli" {
		t.Fatalf("client = %q, want sanitized 'cli'", client)
	}

	// Empty source is a no-op, but client is still overwritten, even to empty.
	if err := st.SetSandboxAttribution(t.Context(), "sb1", "", ""); err != nil {
		t.Fatalf("SetSandboxAttribution (empty): %v", err)
	}
	row, _, _ = st.GetActiveSandboxRow(t.Context(), "sb1")
	if row.Source != "agent" {
		t.Fatalf("source after empty-source call = %q, want unchanged 'agent'", row.Source)
	}
	client = "unchecked"
	if err := st.queryRow(t.Context(), `SELECT client FROM sandboxes WHERE id = ?`, "sb1").Scan(&client); err != nil {
		t.Fatalf("read client: %v", err)
	}
	if client != "" {
		t.Fatalf("client after empty call = %q, want ''", client)
	}

	long := "this-is-a-very-long-client-tag-name"
	if err := st.SetSandboxAttribution(t.Context(), "sb1", "", long); err != nil {
		t.Fatal(err)
	}
	if err := st.queryRow(t.Context(), `SELECT client FROM sandboxes WHERE id = ?`, "sb1").Scan(&client); err != nil {
		t.Fatal(err)
	}
	if len(client) != 24 || client != long[:24] {
		t.Fatalf("client = %q (len %d), want first 24 bytes of %q", client, len(client), long)
	}

	// A naive client[:24] would slice through the two-byte rune at index 23.
	multiByte := "aaaaaaaaaaaaaaaaaaaaaaa" + "é"
	if err := st.SetSandboxAttribution(t.Context(), "sb1", "", multiByte); err != nil {
		t.Fatal(err)
	}
	if err := st.queryRow(t.Context(), `SELECT client FROM sandboxes WHERE id = ?`, "sb1").Scan(&client); err != nil {
		t.Fatal(err)
	}
	if len(client) > 24 {
		t.Fatalf("client = %q (len %d), want <= 24 bytes", client, len(client))
	}
	if !utf8.ValidString(client) {
		t.Fatalf("client = %q is not valid UTF-8 (rune split mid-character)", client)
	}
}

func TestSandboxClientInheritedByHistory(t *testing.T) {
	st := openTemp(t)
	now := int64(6_000_000)
	mkSandbox(t, st, "sc1", "u:c", now)
	if err := st.SetSandboxAttribution(t.Context(), "sc1", "agent", "mcp"); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordHistory(t.Context(), HistoryRecord{
		ID: "sc1", OrgID: "u:c", Template: "node", ExitState: "destroyed", CreatedAt: now, DestroyedAt: now + 5,
	}); err != nil {
		t.Fatalf("RecordHistory: %v", err)
	}
	if err := st.DeleteSandbox(t.Context(), "sc1"); err != nil {
		t.Fatal(err)
	}
	var client string
	if err := st.queryRow(t.Context(), `SELECT client FROM sandbox_history WHERE id = ?`, "sc1").Scan(&client); err != nil {
		t.Fatalf("read history client: %v", err)
	}
	if client != "mcp" {
		t.Fatalf("history client = %q, want inherited 'mcp'", client)
	}
}

// Counts span active and destroyed sandboxes; a blank client folds into "unknown".
func TestCreationsByClient(t *testing.T) {
	st := openTemp(t)
	now := int64(7_000_000)

	mkSandbox(t, st, "cc1", "u:d", now)
	if err := st.SetSandboxAttribution(t.Context(), "cc1", "agent", "cli"); err != nil {
		t.Fatal(err)
	}
	mkSandbox(t, st, "cc2", "u:d", now)
	if err := st.SetSandboxAttribution(t.Context(), "cc2", "agent", "cli"); err != nil {
		t.Fatal(err)
	}
	mkSandbox(t, st, "cc3", "u:d", now)
	if err := st.SetSandboxAttribution(t.Context(), "cc3", "human", "web"); err != nil {
		t.Fatal(err)
	}
	mkSandbox(t, st, "cc4", "u:d", now)

	mkSandbox(t, st, "cc5", "u:d", now)
	if err := st.SetSandboxAttribution(t.Context(), "cc5", "agent", "sdk"); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordHistory(t.Context(), HistoryRecord{
		ID: "cc5", OrgID: "u:d", Template: "node", ExitState: "destroyed", CreatedAt: now, DestroyedAt: now + 5,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSandbox(t.Context(), "cc5"); err != nil {
		t.Fatal(err)
	}

	mkSandbox(t, st, "cc-other", "u:other", now)
	if err := st.SetSandboxAttribution(t.Context(), "cc-other", "agent", "cli"); err != nil {
		t.Fatal(err)
	}

	got, err := st.CreationsByClient(t.Context(), "u:d")
	if err != nil {
		t.Fatalf("CreationsByClient: %v", err)
	}
	want := map[string]int{"cli": 2, "web": 1, "unknown": 1, "sdk": 1}
	if len(got) != len(want) {
		t.Fatalf("CreationsByClient = %+v, want %+v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("CreationsByClient[%q] = %d, want %d (full: %+v)", k, got[k], v, got)
		}
	}
}

func mkSandbox(t *testing.T, st *Store, id, org string, now int64) {
	t.Helper()
	if err := st.SaveSandbox(t.Context(), SandboxRecord{
		ID: id, OrgID: org, Template: "node", State: "running",
		CreatedAt: now, ExpiresAt: now + 3600, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("SaveSandbox %s: %v", id, err)
	}
}

func historySource(recs []HistoryRecord) string {
	if len(recs) > 0 {
		return recs[0].Source
	}
	return ""
}
