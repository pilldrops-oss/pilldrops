package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// TestHistoryEndpoints checks the list is org-scoped and omits logs, the detail
// includes logs, and another org's record is a 404.
func TestHistoryEndpoints(t *testing.T) {
	srv, st, key := newTestServerWithStore(t) // key is scoped to org "default"

	mine := store.HistoryRecord{
		ID: "sb-mine", OrgID: "default", Template: "python-3.11",
		ExitState: "destroyed", CreatedAt: 1000, DestroyedAt: 1042,
		DurationSecs: 42, VCPU: 2, MemMiB: 512, Logs: "hello from mine",
	}
	theirs := store.HistoryRecord{
		ID: "sb-theirs", OrgID: "other", Template: "node",
		ExitState: "crashed", CreatedAt: 2000, DestroyedAt: 2010,
		DurationSecs: 10, Logs: "secret tenant logs",
	}
	if err := st.RecordHistory(t.Context(), mine); err != nil {
		t.Fatalf("record mine: %v", err)
	}
	if err := st.RecordHistory(t.Context(), theirs); err != nil {
		t.Fatalf("record theirs: %v", err)
	}

	get := func(path string) (*http.Response, []byte) {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, b
	}

	resp, body := get("/v1/history")
	if resp.StatusCode != 200 {
		t.Fatalf("list status = %d, want 200 (%s)", resp.StatusCode, body)
	}
	var list struct {
		History []store.HistoryRecord `json:"history"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(list.History) != 1 {
		t.Fatalf("list len = %d, want 1 (org isolation) — %s", len(list.History), body)
	}
	if got := list.History[0].ID; got != "sb-mine" {
		t.Fatalf("list[0].id = %q, want sb-mine", got)
	}
	if list.History[0].Logs != "" {
		t.Fatalf("list must omit logs, got %q", list.History[0].Logs)
	}

	resp, body = get("/v1/history/sb-mine")
	if resp.StatusCode != 200 {
		t.Fatalf("detail status = %d, want 200 (%s)", resp.StatusCode, body)
	}
	var rec store.HistoryRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		t.Fatalf("unmarshal detail: %v", err)
	}
	if rec.Logs != "hello from mine" {
		t.Fatalf("detail logs = %q, want retained logs", rec.Logs)
	}

	resp, body = get("/v1/history/sb-theirs")
	if resp.StatusCode != 404 {
		t.Fatalf("cross-tenant get status = %d, want 404 (%s)", resp.StatusCode, body)
	}
}
