package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

func do(t *testing.T, method, url, key, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, bytes.NewBufferString(body))
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

func TestSandboxLifecycleOverHTTP(t *testing.T) {
	srv, key := newTestServer(t)

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"python-3.11"}`)
	if code != 201 {
		t.Fatalf("create = %d (%v)", code, out)
	}
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("no id in %v", out)
	}

	code, out = do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/exec", key, `{"cmd":"echo hi"}`)
	if code != 200 || out["stdout"] != "hi\n" {
		t.Fatalf("exec = %d %v", code, out)
	}

	if code, _ := do(t, "GET", srv.URL+"/v1/sandboxes/"+id, key, ""); code != 200 {
		t.Fatalf("get = %d", code)
	}
	if code, out := do(t, "GET", srv.URL+"/v1/sandboxes", key, ""); code != 200 || out["sandboxes"] == nil {
		t.Fatalf("list = %d %v", code, out)
	}

	if code, _ := do(t, "DELETE", srv.URL+"/v1/sandboxes/"+id, key, ""); code != 200 {
		t.Fatalf("destroy = %d", code)
	}
	if code, _ := do(t, "GET", srv.URL+"/v1/sandboxes/"+id, key, ""); code != 404 {
		t.Fatalf("get-after-destroy = %d, want 404", code)
	}
}

func TestStartServerOnExistingSandbox(t *testing.T) {
	srv, key := newTestServer(t)
	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base"}`)
	if code != 201 {
		t.Fatalf("create = %d (%v)", code, out)
	}
	id, _ := out["id"].(string)

	if code, _ := do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/start", key, `{}`); code != 400 {
		t.Fatalf("start without cmd = %d, want 400", code)
	}

	code, out = do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/start", key, `{"cmd":"sleep 1"}`)
	if code != 200 {
		t.Fatalf("start = %d (%v)", code, out)
	}
	if out["started"] != "sleep 1" {
		t.Fatalf("started = %v, want 'sleep 1'", out["started"])
	}
	if out["port"] != float64(8000) {
		t.Fatalf("port = %v, want 8000", out["port"])
	}

	if code, _ := do(t, "POST", srv.URL+"/v1/sandboxes/nope/start", key, `{"cmd":"x"}`); code != 404 {
		t.Fatalf("start on unknown = %d, want 404", code)
	}
}

func TestSandboxLogsServeAppOutput(t *testing.T) {
	srv, key := newTestServer(t)

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key,
		`{"template":"python-3.11","start":"echo booted-ok; echo warn-line >&2; sleep 30","port":8000}`)
	if code != 201 {
		t.Fatalf("create = %d (%v)", code, out)
	}
	id, _ := out["id"].(string)

	var lines []any
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		code, out = do(t, "GET", srv.URL+"/v1/sandboxes/"+id+"/logs", key, "")
		if code != 200 {
			t.Fatalf("logs = %d (%v)", code, out)
		}
		lines, _ = out["lines"].([]any)
		if len(lines) >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	joined := fmt.Sprint(lines...)
	if !strings.Contains(joined, "booted-ok") || !strings.Contains(joined, "warn-line") {
		t.Fatalf("logs missing app output; got %v", lines)
	}

	if code, _ := do(t, "GET", srv.URL+"/v1/sandboxes/sb_nope/logs", key, ""); code != 404 {
		t.Fatalf("logs unknown = %d, want 404", code)
	}
}

// A template with a built-in app port (desktop: 8000, the one app port every
// template now publishes) publishes it; an explicit request port wins.
func TestCreatePublishesTemplatePort(t *testing.T) {
	srv, key := newTestServer(t)

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"desktop"}`)
	if code != 201 {
		t.Fatalf("create desktop = %d (%v)", code, out)
	}
	id, _ := out["id"].(string)
	url, _ := out["url"].(string)
	// The desktop template declares the platform desktop port, because its
	// preview IS the viewer envd serves; nothing binds 8000 in it.
	want := "/preview/" + id + "/" + strconv.Itoa(rt.DesktopPort) + "/"
	if !strings.Contains(url, want) {
		t.Fatalf("create url = %q, want preview link containing %q", url, want)
	}
	if code, out = do(t, "GET", srv.URL+"/v1/sandboxes/"+id, key, ""); code != 200 {
		t.Fatalf("get = %d", code)
	}
	if got, _ := out["url"].(string); !strings.Contains(got, "/"+strconv.Itoa(rt.DesktopPort)+"/") {
		t.Fatalf("get url = %q, want port %d", got, rt.DesktopPort)
	}

	code, out = do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"desktop","port":3000}`)
	if code != 201 {
		t.Fatalf("create desktop port=3000 = %d (%v)", code, out)
	}
	if url, _ := out["url"].(string); !strings.Contains(url, "/3000/") {
		t.Fatalf("create url = %q, want port 3000", url)
	}

	// No declared port means no url in the create response.
	code, out = do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base"}`)
	if code != 201 {
		t.Fatalf("create base = %d (%v)", code, out)
	}
	if _, ok := out["url"]; ok {
		t.Fatalf("base create unexpectedly published a url: %v", out["url"])
	}
}

// A template alias (python-3.11 is code-interpreter) resolves before the rootfs lookup.
func TestCreateResolvesTemplateAlias(t *testing.T) {
	srv, key := newTestServer(t)
	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"python-3.11"}`)
	if code != 201 {
		t.Fatalf("create alias = %d (%v)", code, out)
	}
	if tmpl, _ := out["template"].(string); tmpl != "code-interpreter" {
		t.Fatalf("template = %q, want code-interpreter (alias resolved)", tmpl)
	}
}

// An abandoned sandbox must self-expire quickly; the owner extends via /refresh.
func TestDefaultLeaseIsShort(t *testing.T) {
	srv, key := newTestServer(t)
	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base"}`)
	if code != 201 {
		t.Fatalf("create = %d (%v)", code, out)
	}
	created, _ := time.Parse(time.RFC3339, out["created_at"].(string))
	expires, _ := time.Parse(time.RFC3339, out["expires_at"].(string))
	lease := expires.Sub(created)
	if lease > 30*time.Minute || lease <= 0 {
		t.Fatalf("default lease = %s, want a short lease (<=30m)", lease)
	}
}

func TestExecPerCommandEnv(t *testing.T) {
	srv, key := newTestServer(t)
	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"python-3.11"}`)
	if code != 201 {
		t.Fatalf("create = %d (%v)", code, out)
	}
	id, _ := out["id"].(string)

	code, out = do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/exec", key,
		`{"cmd":"echo V=$CENV","env":{"CENV":"per-exec-42"}}`)
	if code != 200 || out["stdout"] != "V=per-exec-42\n" {
		t.Fatalf("exec with env = %d %v, want stdout V=per-exec-42", code, out)
	}
}

// An image-backed service create with no explicit size must not fall to the
// 128 MiB floor, which OOM-kills mysql:8 at init.
func TestCreateImageBackedGetsServiceMemory(t *testing.T) {
	srv, key := newTestServer(t)
	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key,
		`{"template":"mysql","image":"mysql:8"}`)
	if code != 201 {
		t.Fatalf("create image-backed = %d (%v)", code, out)
	}
	id, _ := out["id"].(string)

	code, m := do(t, "GET", srv.URL+"/v1/sandboxes/"+id+"/metrics", key, "")
	if code != 200 {
		t.Fatalf("metrics = %d (%v)", code, m)
	}
	mem, _ := m["mem_mib"].(float64)
	if int(mem) == 128 {
		t.Fatalf("image-backed create got the 128 MiB floor")
	}
	if int(mem) < 512 {
		t.Fatalf("image-backed mem_mib = %d, want a service-appropriate default (>=512)", int(mem))
	}
}

func TestCreateImageBackedRespectsExplicitMemory(t *testing.T) {
	srv, key := newTestServer(t)
	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key,
		`{"template":"redis","image":"redis:7","memory_mb":512}`)
	if code != 201 {
		t.Fatalf("create = %d (%v)", code, out)
	}
	id, _ := out["id"].(string)
	_, m := do(t, "GET", srv.URL+"/v1/sandboxes/"+id+"/metrics", key, "")
	if mem, _ := m["mem_mib"].(float64); int(mem) != 512 {
		t.Fatalf("explicit mem_mib = %d, want 512", int(mem))
	}
}

// Same ruling as cron's TestCronRefusesAgentMode: this server runs no agents.
func TestCreateRefusesAgentMode(t *testing.T) {
	srv, key := newTestServer(t)
	for _, mode := range []string{"supervised", "guided", "autonomous"} {
		code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key,
			`{"template":"base","agent_mode":"`+mode+`"}`)
		if code != http.StatusBadRequest {
			t.Fatalf("agent_mode %q = %d %v, want 400", mode, code, out)
		}
		if msg, _ := out["error"].(string); !strings.Contains(msg, "agent_mode is not supported") {
			t.Errorf("agent_mode %q error = %q", mode, msg)
		}
	}
}
