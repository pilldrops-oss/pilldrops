package httpapi

import (
	"strings"
	"testing"
)

func TestAdminSandboxDrilldown(t *testing.T) {
	srv, st, userKey := newTestServerWithStore(t)
	// Operator key in a different org, so this proves cross-org access.
	_, opKey, err := st.CreateKeyWithRole(t.Context(), "op", "op-org", "operator")
	if err != nil {
		t.Fatal(err)
	}

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", userKey,
		`{"template":"python-3.11","client":"web"}`)
	if code != 201 {
		t.Fatalf("create = %d %v", code, out)
	}
	id, _ := out["id"].(string)
	if code, _ := do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/exec", userKey, `{"cmd":"echo traced"}`); code != 200 {
		t.Fatalf("exec = %d", code)
	}

	code, det := do(t, "GET", srv.URL+"/v1/admin/sandboxes/"+id, opKey, "")
	if code != 200 || det["state"] != "active" {
		t.Fatalf("active drilldown = %d %v", code, det)
	}
	if det["launch"] == nil || det["activity"] == nil {
		t.Fatalf("missing launch/activity: %v", det)
	}
	launch, _ := det["launch"].(map[string]any)
	if launch["client"] != "web" {
		t.Fatalf("launch = %v", launch)
	}

	if code, _ := do(t, "DELETE", srv.URL+"/v1/sandboxes/"+id, userKey, ""); code != 200 {
		t.Fatalf("destroy = %d", code)
	}
	code, det = do(t, "GET", srv.URL+"/v1/admin/sandboxes/"+id, opKey, "")
	if code != 200 || det["state"] != "destroyed" {
		t.Fatalf("destroyed drilldown = %d %v", code, det)
	}
	// Logs must not be serialized twice.
	if _, isStr := det["logs"].(string); !isStr {
		t.Fatalf("logs not a string: %#v", det["logs"])
	}
	if rec, ok := det["record"].(map[string]any); ok {
		if lv, has := rec["logs"]; has {
			if s, _ := lv.(string); s != "" {
				t.Fatalf("record.logs should be blanked, got %v", lv)
			}
		}
	}
	acts, _ := det["activity"].([]any)
	var actions []string
	for _, a := range acts {
		m, _ := a.(map[string]any)
		actions = append(actions, m["action"].(string))
	}
	joined := strings.Join(actions, ",")
	if !strings.Contains(joined, "sandbox.create") || !strings.Contains(joined, "sandbox.exec") || !strings.Contains(joined, "sandbox.destroy") {
		t.Fatalf("timeline incomplete: %v", actions)
	}

	if c, _ := do(t, "GET", srv.URL+"/v1/admin/sandboxes/sb-nope", opKey, ""); c != 404 {
		t.Fatalf("unknown = %d, want 404", c)
	}
	if c, _ := do(t, "GET", srv.URL+"/v1/admin/sandboxes/"+id, userKey, ""); c != 403 {
		t.Fatalf("user key = %d, want 403", c)
	}
}

// Pruned history with surviving audit rows yields a timeline-only "unknown", not 404.
func TestAdminSandboxUnknownState(t *testing.T) {
	srv, st, _ := newTestServerWithStore(t)
	_, opKey, err := st.CreateKeyWithRole(t.Context(), "op", "op-org", "operator")
	if err != nil {
		t.Fatal(err)
	}

	if err := st.RecordAudit(t.Context(), "org-x", "k", "sandbox.create", "sb-ghost"); err != nil {
		t.Fatal(err)
	}

	code, det := do(t, "GET", srv.URL+"/v1/admin/sandboxes/sb-ghost", opKey, "")
	if code != 200 {
		t.Fatalf("unknown-state drilldown = %d %v", code, det)
	}
	if det["state"] != "unknown" {
		t.Fatalf("state = %v, want unknown", det["state"])
	}
	if rec, ok := det["record"]; ok && rec != nil {
		t.Fatalf("record = %v, want nil/absent", rec)
	}
	if v, has := det["launch"]; has && v != nil {
		t.Fatalf("launch present: %v, want absent (pre-v9 create has no detail)", v)
	}
	acts, _ := det["activity"].([]any)
	if len(acts) != 1 {
		t.Fatalf("activity = %v, want 1 row", acts)
	}
	row, _ := acts[0].(map[string]any)
	if row["org_id"] != "org-x" {
		t.Fatalf("activity[0].org_id = %v, want org-x", row["org_id"])
	}
	if v, has := row["detail"]; has && v != nil {
		t.Fatalf("activity[0].detail present: %v, want absent", v)
	}

	if c, _ := do(t, "GET", srv.URL+"/v1/admin/sandboxes/sb-nope-at-all", opKey, ""); c != 404 {
		t.Fatalf("unknown = %d, want 404", c)
	}
}
