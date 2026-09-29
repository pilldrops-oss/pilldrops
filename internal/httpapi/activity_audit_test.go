//go:build !core

package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// findDetail returns the parsed detail JSON of the first entry with action, or
// fails the test.
func findDetail(t *testing.T, entries []store.AuditEntry, action string) map[string]any {
	t.Helper()
	for _, e := range entries {
		if e.Action == action {
			var m map[string]any
			if err := json.Unmarshal([]byte(e.Detail), &m); err != nil {
				t.Fatalf("%s detail %q: %v", action, e.Detail, err)
			}
			return m
		}
	}
	t.Fatalf("no %s entry in %+v", action, entries)
	return nil
}

// Create audit carries env keys and file paths only; exec carries cmd and exit code.
func TestCreateAndExecAuditDetail(t *testing.T) {
	srv, st, key := newTestServerWithStore(t)

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key,
		`{"template":"python-3.11","vcpu":1,"env":["SECRET_TOKEN=hunter2"],`+
			`"files":[{"path":"app.py","content":"print(1)"}],"setup":["pip install flask"],`+
			`"start":"python app.py","port":8000,"client":"web"}`)
	if code != 201 {
		t.Fatalf("create = %d %v", code, out)
	}
	id, _ := out["id"].(string)

	entries, err := st.ListAuditByTarget(t.Context(), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	d := findDetail(t, entries, "sandbox.create")
	// The audit records the canonical template id, not the caller's alias.
	if d["template"] != "code-interpreter" || d["start_cmd"] != "python app.py" || d["client"] != "web" {
		t.Fatalf("create detail = %v", d)
	}
	// mem_mb was not requested, so it must be the resolved default, not zero.
	if d["vcpu"] != float64(1) {
		t.Fatalf("vcpu = %v, want 1", d["vcpu"])
	}
	if memMB, ok := d["mem_mb"].(float64); !ok || memMB <= 0 {
		t.Fatalf("mem_mb = %v, want a positive resolved default", d["mem_mb"])
	}
	raw, _ := json.Marshal(d)
	if !strings.Contains(string(raw), "SECRET_TOKEN") || strings.Contains(string(raw), "hunter2") {
		t.Fatalf("env redaction broken: %s", raw)
	}
	if strings.Contains(string(raw), "print(1)") {
		t.Fatalf("file content leaked: %s", raw)
	}

	big := strings.Repeat("x", 5000)
	if code, _ := do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/exec", key,
		`{"cmd":"echo hi # `+big+`"}`); code != 200 {
		t.Fatalf("exec = %d", code)
	}
	entries, _ = st.ListAuditByTarget(t.Context(), id, 0)
	e := findDetail(t, entries, "sandbox.exec")
	cmd, _ := e["cmd"].(string)
	if len(cmd) != maxAuditCmdBytes {
		t.Fatalf("cmd len = %d, want %d", len(cmd), maxAuditCmdBytes)
	}
	if _, ok := e["exit_code"]; !ok {
		t.Fatalf("no exit_code in %v", e)
	}
	if _, ok := e["duration_ms"]; !ok {
		t.Fatalf("no duration_ms in %v", e)
	}
}

func TestExecAuditCmdTruncationIsUTF8Safe(t *testing.T) {
	srv, st, key := newTestServerWithStore(t)

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"python-3.11"}`)
	if code != 201 {
		t.Fatalf("create = %d %v", code, out)
	}
	id, _ := out["id"].(string)

	// Puts a multi-byte rune straddling the byte cap.
	cmd := strings.Repeat("x", 2047) + "世界"
	if code, _ := do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/exec", key,
		`{"cmd":"`+cmd+`"}`); code != 200 {
		t.Fatalf("exec = %d", code)
	}

	entries, err := st.ListAuditByTarget(t.Context(), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	e := findDetail(t, entries, "sandbox.exec")
	got, _ := e["cmd"].(string)
	if len(got) > maxAuditCmdBytes {
		t.Fatalf("cmd len = %d, want <= %d", len(got), maxAuditCmdBytes)
	}
	if strings.ContainsRune(got, '�') {
		t.Fatalf("cmd contains a replacement rune (split multi-byte rune): %q", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("cmd is not valid UTF-8: %q", got)
	}
}

func TestFileAndLifecycleAuditDetail(t *testing.T) {
	srv, st, key := newTestServerWithStore(t)

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"python-3.11"}`)
	if code != 201 {
		t.Fatalf("create = %d %v", code, out)
	}
	id, _ := out["id"].(string)

	if code, _ := doRaw(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/files?path=notes.txt", key, "hello world"); code != 200 {
		t.Fatalf("write = %d", code)
	}
	if code, _ := do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/filesystem/mkdir", key, `{"path":"newdir"}`); code != 200 {
		t.Fatalf("mkdir = %d", code)
	}
	if code, _ := do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/filesystem/move", key, `{"src":"notes.txt","dst":"notes2.txt"}`); code != 200 {
		t.Fatalf("move = %d", code)
	}
	if code, _ := do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/filesystem/remove", key, `{"path":"newdir"}`); code != 200 {
		t.Fatalf("remove = %d", code)
	}
	if code, _ := do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/start", key,
		`{"cmd":"sleep 60","port":8000,"env":{"TOKEN":"leakme"}}`); code != 200 {
		t.Fatalf("start = %d", code)
	}
	if code, _ := do(t, "DELETE", srv.URL+"/v1/sandboxes/"+id, key, ""); code != 200 {
		t.Fatalf("destroy = %d", code)
	}

	entries, err := st.ListAuditByTarget(t.Context(), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if d := findDetail(t, entries, "sandbox.fs.write"); d["path"] != "notes.txt" || d["size"] != float64(len("hello world")) {
		t.Fatalf("fs.write detail = %v", d)
	}
	if d := findDetail(t, entries, "sandbox.fs.mkdir"); d["path"] != "newdir" {
		t.Fatalf("fs.mkdir detail = %v", d)
	}
	if d := findDetail(t, entries, "sandbox.fs.move"); d["src"] != "notes.txt" || d["dst"] != "notes2.txt" {
		t.Fatalf("fs.move detail = %v", d)
	} else if _, hasPath := d["path"]; hasPath {
		t.Fatalf("fs.move detail should not have a path key: %v", d)
	}
	if d := findDetail(t, entries, "sandbox.fs.remove"); d["path"] != "newdir" {
		t.Fatalf("fs.remove detail = %v", d)
	}
	d := findDetail(t, entries, "sandbox.start")
	if d["cmd"] != "sleep 60" {
		t.Fatalf("start detail = %v", d)
	}
	raw, _ := json.Marshal(d)
	if !strings.Contains(string(raw), "TOKEN") || strings.Contains(string(raw), "leakme") {
		t.Fatalf("start env redaction broken: %s", raw)
	}
	if d := findDetail(t, entries, "sandbox.destroy"); d["reason"] != "user" {
		t.Fatalf("destroy detail = %v", d)
	}
	for _, e := range entries {
		if strings.Contains(e.Detail, "hello world") {
			t.Fatalf("file content leaked: %+v", e)
		}
	}
}

// doRaw posts a raw, non-JSON body.
func doRaw(t *testing.T, method, url, key, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}
