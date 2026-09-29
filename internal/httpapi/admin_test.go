package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/store"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

func TestKeysCRUDOverHTTP(t *testing.T) {
	srv, key := newTestServer(t)
	if code, _ := do(t, "GET", srv.URL+"/v1/keys", key, ""); code != 200 {
		t.Fatalf("list keys = %d", code)
	}
	code, out := do(t, "POST", srv.URL+"/v1/keys", key, `{"name":"ci"}`)
	if code != 201 || out["key"] == nil {
		t.Fatalf("create key = %d %v", code, out)
	}
	newID, _ := out["id"].(string)
	if code, _ := do(t, "DELETE", srv.URL+"/v1/keys/"+newID, key, ""); code != 200 {
		t.Fatalf("delete key = %d", code)
	}
}

// Cross-org revoke must 404, not 403, so the id's existence is not confirmed.
func TestDeleteKeyCannotReachAnotherOrg(t *testing.T) {
	srv, st, attackerKey := newTestServerWithStore(t)
	victim, victimPlaintext, err := st.CreateKey(t.Context(), "victim-prod", "u:victim")
	if err != nil {
		t.Fatalf("victim key: %v", err)
	}

	if code, _ := do(t, "DELETE", srv.URL+"/v1/keys/"+victim.ID, attackerKey, ""); code != http.StatusNotFound {
		t.Fatalf("cross-org revoke = %d, want 404", code)
	}
	if _, err := st.ValidateKey(t.Context(), victimPlaintext); err != nil {
		t.Fatalf("another org revoked the victim's key: %v", err)
	}

	if code, _ := do(t, "DELETE", srv.URL+"/v1/keys/"+victim.ID, victimPlaintext, ""); code != 200 {
		t.Fatalf("owner revoke = %d, want 200", code)
	}
	if _, err := st.ValidateKey(t.Context(), victimPlaintext); err == nil {
		t.Fatal("owner's revoke did not take effect")
	}
}

func TestAdminSandboxListSpansAllOrgs(t *testing.T) {
	srv, st, userKey := newTestServerWithStore(t)
	_, custKey, err := st.CreateKey(t.Context(), "cust", "u:cust1")
	if err != nil {
		t.Fatalf("customer key: %v", err)
	}
	_, opKey, err := st.CreateKeyWithRole(t.Context(), "op", "default", "operator")
	if err != nil {
		t.Fatalf("operator key: %v", err)
	}

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", userKey, `{"template":"base"}`)
	if code != 201 {
		t.Fatalf("create (default org) = %d %v", code, out)
	}
	defaultID, _ := out["id"].(string)
	code, out = do(t, "POST", srv.URL+"/v1/sandboxes", custKey, `{"template":"base"}`)
	if code != 201 {
		t.Fatalf("create (customer org) = %d %v", code, out)
	}
	custID, _ := out["id"].(string)

	_, out = do(t, "GET", srv.URL+"/v1/sandboxes", opKey, "")
	if ids := sandboxIDs(out); len(ids) != 1 || ids[0] != defaultID {
		t.Fatalf("org-scoped list = %v, want just the operator org's own %s", ids, defaultID)
	}

	_, out = do(t, "GET", srv.URL+"/v1/admin/sandboxes", opKey, "")
	ids := sandboxIDs(out)
	for _, want := range []string{defaultID, custID} {
		found := false
		for _, id := range ids {
			if id == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("admin inventory %v missing %s — the reconciler cannot rebuild what it cannot see", ids, want)
		}
	}

	if code, _ := do(t, "GET", srv.URL+"/v1/admin/sandboxes", userKey, ""); code != http.StatusForbidden {
		t.Fatalf("user key on /v1/admin/sandboxes = %d, want 403", code)
	}
}

// sandboxIDs pulls the ids out of a {"sandboxes":[...]} response.
func sandboxIDs(out map[string]any) []string {
	list, _ := out["sandboxes"].([]any)
	ids := make([]string, 0, len(list))
	for _, raw := range list {
		if sb, ok := raw.(map[string]any); ok {
			if id, _ := sb["id"].(string); id != "" {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func TestUsageAndTemplates(t *testing.T) {
	srv, key := newTestServer(t)
	code, out := do(t, "GET", srv.URL+"/v1/usage", key, "")
	if code != 200 {
		t.Fatalf("usage = %d", code)
	}
	if _, ok := out["vcpu_hours"]; !ok {
		t.Fatalf("usage missing vcpu_hours: %v", out)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/v1/templates", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/templates: %v", err)
	}
	defer resp.Body.Close()
	var arr []map[string]any
	json.NewDecoder(resp.Body).Decode(&arr)
	if want := len(templates.Catalog()); len(arr) != want {
		t.Fatalf("templates = %d, want %d (catalog size)", len(arr), want)
	}
	var haveClaude, variedMem bool
	firstMem := arr[0]["memory_mb"]
	for _, tpl := range arr {
		if tpl["id"] == "claude" {
			haveClaude = true
		}
		if tpl["memory_mb"] != firstMem {
			variedMem = true
		}
	}
	if !haveClaude {
		t.Fatalf("catalog missing claude template: %v", arr)
	}
	if !variedMem {
		t.Fatalf("templates share identical memory_mb; catalog metadata not wired")
	}
	// An unbuilt route refuses rather than claiming the org has no members.
	if code, body := do(t, "GET", srv.URL+"/v1/members", key, ""); code != http.StatusNotImplemented {
		t.Fatalf("/v1/members = %d %s, want 501: an empty list would advertise a feature that does not exist", code, body)
	}
}

// total_starts counts distinct metered sandboxes from the store, not what is running now.
func TestUsageTotalStartsSurvivesDestroy(t *testing.T) {
	srv, st, key := newTestServerWithStore(t)
	if err := st.RecordUsage(t.Context(), "sb-dead-1", "default", store.UsageDelta{VCPUSeconds: 60, RAMGBSeconds: 60}); err != nil {
		t.Fatalf("record usage: %v", err)
	}
	if err := st.RecordUsage(t.Context(), "sb-dead-1", "default", store.UsageDelta{VCPUSeconds: 30, RAMGBSeconds: 30}); err != nil {
		t.Fatalf("record usage: %v", err)
	}
	if err := st.RecordUsage(t.Context(), "sb-dead-2", "default", store.UsageDelta{VCPUSeconds: 60, RAMGBSeconds: 60}); err != nil {
		t.Fatalf("record usage: %v", err)
	}
	code, out := do(t, "GET", srv.URL+"/v1/usage", key, "")
	if code != 200 {
		t.Fatalf("usage = %d", code)
	}
	if got, _ := out["total_starts"].(float64); got != 2 {
		t.Fatalf("total_starts = %v, want 2 (distinct metered sandboxes; nothing is running)", out["total_starts"])
	}
	if got, _ := out["vcpu_hours"].(float64); got == 0 {
		t.Fatalf("vcpu_hours = 0, want nonzero from recorded deltas")
	}
}

// Chart points come from durable state, not a node-local sampler.
func TestUsagePointsDeriveFromStore(t *testing.T) {
	srv, st, key := newTestServerWithStore(t)
	now := time.Now().Unix()
	if err := st.RecordHistory(t.Context(), store.HistoryRecord{
		ID: "sb-span", OrgID: "default", Template: "base", ExitState: "destroyed",
		CreatedAt: now - 40, DestroyedAt: now - 20, DurationSecs: 20, VCPU: 1, MemMiB: 1024,
	}); err != nil {
		t.Fatalf("record history: %v", err)
	}
	if err := st.SaveSandbox(t.Context(), store.SandboxRecord{
		ID: "sb-live", OrgID: "default", Template: "base", State: "running", CreatedAt: now - 5,
	}); err != nil {
		t.Fatalf("save sandbox: %v", err)
	}

	code, out := do(t, "GET", srv.URL+"/v1/usage", key, "")
	if code != 200 {
		t.Fatalf("usage = %d", code)
	}
	if got, _ := out["active"].(float64); got != 1 {
		t.Fatalf("active = %v, want 1 (fleet-wide from the store, not the serving node)", out["active"])
	}
	raw, _ := out["points"].([]any)
	if len(raw) != 60 {
		t.Fatalf("points = %d, want 60 (last minute at 1s resolution)", len(raw))
	}
	starts, maxConc := 0.0, 0.0
	var lastConc float64
	for _, p := range raw {
		m := p.(map[string]any)
		c, _ := m["concurrent"].(float64)
		s, _ := m["start_rate"].(float64)
		starts += s
		if c > maxConc {
			maxConc = c
		}
		lastConc = c
	}
	if starts < 2 {
		t.Fatalf("sum(start_rate) = %v, want >= 2 (one historic + one running start in window)", starts)
	}
	if maxConc < 1 {
		t.Fatalf("max concurrent = %v, want >= 1", maxConc)
	}
	if lastConc < 1 {
		t.Fatalf("latest concurrent = %v, want >= 1 (sb-live is still running)", lastConc)
	}
}

func TestUsageByClient(t *testing.T) {
	srv, _, key := newTestServerWithStore(t)

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"python-3.11","client":"cli"}`)
	if code != 201 {
		t.Fatalf("create = %d %v", code, out)
	}
	code, out = do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"python-3.11","client":"web"}`)
	if code != 201 {
		t.Fatalf("create = %d %v", code, out)
	}

	code, out = do(t, "GET", srv.URL+"/v1/usage", key, "")
	if code != 200 {
		t.Fatalf("usage = %d %v", code, out)
	}
	byClient, ok := out["by_client"].(map[string]any)
	if !ok {
		t.Fatalf("usage missing by_client (or wrong shape): %v", out)
	}
	if v, _ := byClient["cli"].(float64); v != 1 {
		t.Fatalf("by_client.cli = %v, want 1: %v", byClient["cli"], byClient)
	}
	if v, _ := byClient["human"].(float64); v != 0 {
		// by_client keys on the raw client tag, not the classified source.
		t.Fatalf("by_client should key on the raw client tag, not the classified source: %v", byClient)
	}
	if v, _ := byClient["web"].(float64); v != 1 {
		t.Fatalf("by_client.web = %v, want 1: %v", byClient["web"], byClient)
	}
}

func TestAuthCacheServesRepeatCalls(t *testing.T) {
	srv, st, key := newTestServerWithStore(t)
	if code, _ := do(t, "GET", srv.URL+"/v1/usage", key, ""); code != 200 {
		t.Fatalf("first call = %d", code)
	}
	// Revoke behind the cache's back; within the TTL the key must still work.
	keys, err := st.ListKeys(t.Context(), "default")
	if err != nil || len(keys) == 0 {
		t.Fatalf("list keys: %v %d", err, len(keys))
	}
	if err := st.RevokeKey(t.Context(), keys[0].ID, "default"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if code, _ := do(t, "GET", srv.URL+"/v1/usage", key, ""); code != 200 {
		t.Fatalf("cached call after external revoke = %d, want 200 (cache hit)", code)
	}
}
