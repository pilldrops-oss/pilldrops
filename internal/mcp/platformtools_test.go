package mcp

import (
	"strings"
	"testing"
)

// Every platform tool must reach the route it claims to. A tool that builds
// the wrong path, or sends a field the handler ignores, is indistinguishable
// from a working one until an agent uses it, so the call itself is asserted.
func TestPlatformToolsCallTheRouteTheyClaim(t *testing.T) {
	for name, tc := range map[string]struct {
		tool               string
		args               map[string]any
		method, path, body string
	}{
		"save_checkpoint": {
			tool: "save_checkpoint", args: map[string]any{"sandbox_id": "sb_1", "name": "before-migration"},
			method: "POST", path: "/v1/sandboxes/sb_1/checkpoints", body: `{"name":"before-migration"}`,
		},
		"create_backup": {
			tool: "create_backup", args: map[string]any{"sandbox_id": "sb_1"},
			method: "POST", path: "/v1/sandboxes/sb_1/backups", body: "",
		},
		"restore from a checkpoint": {
			tool: "restore_sandbox",
			args: map[string]any{"sandbox_id": "sb_1", "checkpoint": "before-migration", "confirm": true},
			// The checkpoint route decodes no body, so none is sent.
			method: "POST", path: "/v1/sandboxes/sb_1/checkpoints/before-migration/restore", body: "",
		},
		"restore from a backup carries the confirm the route requires": {
			tool:   "restore_sandbox",
			args:   map[string]any{"sandbox_id": "sb_1", "backup": "bkp_9", "confirm": true},
			method: "POST", path: "/v1/sandboxes/sb_1/backups/bkp_9/restore",
			body: `{"confirm":true}`,
		},
		"delete a checkpoint": {
			tool:   "delete_saved_state",
			args:   map[string]any{"sandbox_id": "sb_1", "checkpoint": "old", "confirm": "true"},
			method: "DELETE", path: "/v1/sandboxes/sb_1/checkpoints/old", body: "",
		},
		"delete a backup": {
			tool:   "delete_saved_state",
			args:   map[string]any{"sandbox_id": "sb_1", "backup": "bkp_9", "confirm": true},
			method: "DELETE", path: "/v1/sandboxes/sb_1/backups/bkp_9", body: "",
		},
		"save_snapshot": {
			tool: "save_snapshot", args: map[string]any{"sandbox_id": "sb_1", "name": "warm"},
			method: "POST", path: "/v1/sandboxes/sb_1/snapshot", body: `{"name":"warm"}`,
		},
		"list_snapshots": {
			tool: "list_snapshots", args: map[string]any{},
			method: "GET", path: "/v1/snapshots",
		},
		"list_snapshots for one name": {
			tool: "list_snapshots", args: map[string]any{"name": "warm"},
			method: "GET", path: "/v1/snapshots/warm",
		},
		"delete_snapshot": {
			tool: "delete_snapshot", args: map[string]any{"name": "warm", "confirm": true},
			method: "DELETE", path: "/v1/snapshots/warm",
		},
		"standby_sandbox": {
			tool: "standby_sandbox", args: map[string]any{"sandbox_id": "sb_1"},
			method: "POST", path: "/v1/sandboxes/sb_1/standby", body: "",
		},
		"renew_sandbox with no argument resets the idle clock": {
			tool: "renew_sandbox", args: map[string]any{"sandbox_id": "sb_1"},
			method: "POST", path: "/v1/sandboxes/sb_1/refresh", body: "",
		},
		"renew_sandbox with a lifetime re-stamps the TTL": {
			tool: "renew_sandbox", args: map[string]any{"sandbox_id": "sb_1", "timeout_seconds": float64(3600)},
			method: "POST", path: "/v1/sandboxes/sb_1/timeout", body: `{"seconds":3600}`,
		},
		"renew_sandbox -1 asks for no expiry": {
			tool: "renew_sandbox", args: map[string]any{"sandbox_id": "sb_1", "timeout_seconds": float64(-1)},
			method: "POST", path: "/v1/sandboxes/sb_1/timeout", body: `{"seconds":-1}`,
		},
		"list_files": {
			tool: "list_files", args: map[string]any{"sandbox_id": "sb_1", "path": "/app/src"},
			method: "GET", path: "/v1/sandboxes/sb_1/filesystem/list?path=app%2Fsrc",
		},
		"list_files defaults to the workspace root": {
			tool: "list_files", args: map[string]any{"sandbox_id": "sb_1"},
			method: "GET", path: "/v1/sandboxes/sb_1/filesystem/list?path=.",
		},
		"list_secrets": {
			tool: "list_secrets", args: map[string]any{}, method: "GET", path: "/v1/secrets",
		},
		"put_secret": {
			tool: "put_secret", args: map[string]any{"name": "OPENAI_API_KEY", "value": "sk-x"},
			method: "POST", path: "/v1/secrets", body: `{"name":"OPENAI_API_KEY","value":"sk-x"}`,
		},
		"put_secret with auto_inject": {
			tool: "put_secret", args: map[string]any{"name": "K", "value": "v", "auto_inject": true},
			method: "POST", path: "/v1/secrets", body: `"auto_inject":true`,
		},
		"delete_secret": {
			tool: "delete_secret", args: map[string]any{"name": "OPENAI_API_KEY"},
			method: "DELETE", path: "/v1/secrets/OPENAI_API_KEY",
		},
		"forward_port": {
			tool: "forward_port", args: map[string]any{"sandbox_id": "sb_1", "port": float64(5432)},
			method: "POST", path: "/v1/sandboxes/sb_1/forwards", body: `{"port":5432}`,
		},
		"forward_port with udp": {
			tool: "forward_port", args: map[string]any{"sandbox_id": "sb_1", "port": "5432", "udp": true},
			method: "POST", path: "/v1/sandboxes/sb_1/forwards", body: `"udp":true`,
		},
		"list_port_forwards": {
			tool: "list_port_forwards", args: map[string]any{"sandbox_id": "sb_1"},
			method: "GET", path: "/v1/sandboxes/sb_1/forwards",
		},
		"remove_port_forward addresses the guest port": {
			tool: "remove_port_forward", args: map[string]any{"sandbox_id": "sb_1", "port": float64(5432)},
			method: "DELETE", path: "/v1/sandboxes/sb_1/forwards/5432",
		},
		"add_domain": {
			tool:   "add_domain",
			args:   map[string]any{"host": "app.example.com", "sandbox_id": "sb_1", "port": float64(3000)},
			method: "POST", path: "/v1/domains",
			body: `{"host":"app.example.com","port":3000,"sandbox_id":"sb_1"}`,
		},
		"add_domain wildcard takes no sandbox": {
			tool: "add_domain", args: map[string]any{"host": "*.example.com"},
			method: "POST", path: "/v1/domains", body: `{"host":"*.example.com"}`,
		},
		"list_domains": {
			tool: "list_domains", args: map[string]any{}, method: "GET", path: "/v1/domains",
		},
		"remove_domain": {
			tool: "remove_domain", args: map[string]any{"host": "app.example.com"},
			method: "DELETE", path: "/v1/domains/app.example.com",
		},
	} {
		t.Run(name, func(t *testing.T) {
			be := &recordingBackend{}
			callTool(tc.tool, tc.args, be.fn)
			if len(be.calls) != 1 {
				t.Fatalf("%s made %d backend calls, want 1: %v", tc.tool, len(be.calls), be.calls)
			}
			got := be.calls[0]
			if got.method != tc.method || got.path != tc.path {
				t.Errorf("%s called %s %s, want %s %s", tc.tool, got.method, got.path, tc.method, tc.path)
			}
			if tc.body == "" && got.body != "" {
				t.Errorf("%s sent a body the route does not read: %s", tc.tool, got.body)
			}
			if tc.body != "" && !strings.Contains(got.body, tc.body) {
				t.Errorf("%s body = %s, want it to contain %s", tc.tool, got.body, tc.body)
			}
		})
	}
}

// The schedule write answers with the config view, which omits the schedule;
// only the read carries it. The tool must send the schedule and then report
// what actually stuck, including the cron a period became.
func TestSetBackupScheduleWritesThenReadsBack(t *testing.T) {
	be := &scriptedBackend{bodies: map[string]string{
		"/v1/sandboxes/sb_1/config": `{"backups":{"cron":"0 0 * * *","enabled":true},"env":[]}`,
	}}
	res := callTool("set_backup_schedule", map[string]any{
		"sandbox_id": "sb_1", "period": "24h", "retention": "7d",
	}, be.fn)
	if len(be.calls) != 2 || be.calls[0].method != "POST" || be.calls[1].method != "GET" {
		t.Fatalf("calls = %v, want a POST then a GET of the config", be.calls)
	}
	if got := be.calls[0]; got.path != "/v1/sandboxes/sb_1/config" ||
		got.body != `{"backups":{"period":"24h","retention":"7d"}}` {
		t.Errorf("wrote %s %s, want only the backups key on the config route", got.path, got.body)
	}
	// An env key in the body would merge over the sandbox's configuration.
	if strings.Contains(be.calls[0].body, `"env"`) {
		t.Errorf("body = %s, want it free of env", be.calls[0].body)
	}
	if !strings.Contains(resultText(res), `"cron":"0 0 * * *"`) {
		t.Errorf("result = %s, want the schedule the read reports", resultText(res))
	}
}

// The destructive verbs must be impossible to trip by accident: no confirm
// means no request at all, not a request that happens to be refused upstream.
func TestDestructiveToolsRefuseWithoutConfirm(t *testing.T) {
	for name, args := range map[string]struct {
		tool string
		args map[string]any
	}{
		"restore a checkpoint": {"restore_sandbox", map[string]any{"sandbox_id": "sb_1", "checkpoint": "c"}},
		"restore a backup":     {"restore_sandbox", map[string]any{"sandbox_id": "sb_1", "backup": "b"}},
		"delete a checkpoint":  {"delete_saved_state", map[string]any{"sandbox_id": "sb_1", "checkpoint": "c"}},
		"delete a backup":      {"delete_saved_state", map[string]any{"sandbox_id": "sb_1", "backup": "b"}},
		"delete a snapshot":    {"delete_snapshot", map[string]any{"name": "warm"}},
		"confirm false":        {"delete_snapshot", map[string]any{"name": "warm", "confirm": false}},
		"confirm not a bool":   {"delete_snapshot", map[string]any{"name": "warm", "confirm": "yes please"}},
	} {
		t.Run(name, func(t *testing.T) {
			be := &recordingBackend{}
			res := callTool(args.tool, args.args, be.fn)
			if len(be.calls) != 0 {
				t.Errorf("%s reached the API unconfirmed: %v", args.tool, be.calls)
			}
			if res["isError"] != true {
				t.Errorf("%s answered without an error result: %v", args.tool, res)
			}
			if !strings.Contains(resultText(res), "confirm=true") {
				t.Errorf("%s did not say how to proceed: %s", args.tool, resultText(res))
			}
		})
	}
}

// Naming neither save point, or both, must not pick one silently: either would
// restore or delete a state the caller did not mean.
func TestSavedStateNeedsExactlyOneTarget(t *testing.T) {
	for name, args := range map[string]map[string]any{
		"neither": {"sandbox_id": "sb_1", "confirm": true},
		"both":    {"sandbox_id": "sb_1", "checkpoint": "c", "backup": "b", "confirm": true},
	} {
		for _, tool := range []string{"restore_sandbox", "delete_saved_state"} {
			be := &recordingBackend{}
			res := callTool(tool, args, be.fn)
			if len(be.calls) != 0 {
				t.Errorf("%s/%s reached the API with an ambiguous target: %v", tool, name, be.calls)
			}
			if res["isError"] != true {
				t.Errorf("%s/%s did not report the ambiguity: %v", tool, name, res)
			}
		}
	}
}

func TestListSavedStatesMergesBothStores(t *testing.T) {
	be := &scriptedBackend{bodies: map[string]string{
		"/v1/sandboxes/sb_1/checkpoints": `{"checkpoints":[{"name":"c1"}],"max":5}`,
		"/v1/sandboxes/sb_1/backups":     `{"backups":[{"id":"bkp_1"}]}`,
	}}
	text := resultText(callTool("list_saved_states", map[string]any{"sandbox_id": "sb_1"}, be.fn))
	for _, want := range []string{`"c1"`, `"bkp_1"`, `"max":5`} {
		if !strings.Contains(text, want) {
			t.Errorf("merged view = %s, want it to carry %s", text, want)
		}
	}
}

// A deployment with no object store still has checkpoints; failing the whole
// read there would hide the save points that do exist.
func TestListSavedStatesSurvivesBackupsBeingUnconfigured(t *testing.T) {
	be := &scriptedBackend{
		bodies: map[string]string{"/v1/sandboxes/sb_1/checkpoints": `{"checkpoints":[{"name":"c1"}],"max":5}`},
		status: map[string]int{"/v1/sandboxes/sb_1/backups": 501},
	}
	res := callTool("list_saved_states", map[string]any{"sandbox_id": "sb_1"}, be.fn)
	if res["isError"] == true {
		t.Fatalf("an unconfigured backup store failed the whole read: %v", res)
	}
	text := resultText(res)
	if !strings.Contains(text, `"c1"`) || !strings.Contains(text, "backups_unavailable") {
		t.Errorf("view = %s, want the checkpoints plus a named reason for the missing backups", text)
	}
}

// A checkpoint name is caller-supplied and lands in the path.
func TestSavedStateNamesArePathEscaped(t *testing.T) {
	be := &recordingBackend{}
	callTool("delete_saved_state", map[string]any{
		"sandbox_id": "sb_1", "checkpoint": "../../../v1/admin/plans", "confirm": true,
	}, be.fn)
	if len(be.calls) != 1 {
		t.Fatalf("want 1 call, got %v", be.calls)
	}
	if strings.Contains(be.calls[0].path, "/admin/") {
		t.Errorf("path = %q — a slash in the checkpoint name escaped into the route", be.calls[0].path)
	}
}

// scriptedBackend answers per path, for the one tool that makes two calls.
type scriptedBackend struct {
	calls  []beCall
	bodies map[string]string
	status map[string]int
}

func (b *scriptedBackend) fn(method, path string, body []byte, ctype string) (int, []byte, error) {
	b.calls = append(b.calls, beCall{method, path, string(body), ctype})
	if st, ok := b.status[path]; ok {
		return st, []byte(`{"error":"not configured"}`), nil
	}
	return 200, []byte(b.bodies[path]), nil
}
