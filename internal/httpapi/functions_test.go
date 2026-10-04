package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestFunctionRegisterListGetDelete(t *testing.T) {
	srv, key := newTestServer(t)

	code, out := do(t, "POST", srv.URL+"/v1/functions", key,
		`{"name":"hello","template":"python-3.11","start":"python3 handler.py","port":8000,
		  "files":[{"path":"handler.py","content":"print('hi')"}]}`)
	if code != 201 {
		t.Fatalf("register = %d (%v)", code, out)
	}
	if out["name"] != "hello" {
		t.Fatalf("register body = %v", out)
	}
	if code, _ := do(t, "POST", srv.URL+"/v1/functions", key,
		`{"name":"hello","template":"python-3.11"}`); code != 409 {
		t.Fatalf("dup register = %d, want 409", code)
	}
	if code, _ := do(t, "POST", srv.URL+"/v1/functions", key,
		`{"name":"x","template":"cobol-74"}`); code != 400 {
		t.Fatalf("bad template = %d, want 400", code)
	}
	if code, out := do(t, "GET", srv.URL+"/v1/functions", key, ""); code != 200 || out["functions"] == nil {
		t.Fatalf("list = %d %v", code, out)
	}
	// The alias "python-3.11" is stored as the canonical "code-interpreter".
	if code, out := do(t, "GET", srv.URL+"/v1/functions/hello", key, ""); code != 200 || out["template"] != "code-interpreter" {
		t.Fatalf("get = %d %v", code, out)
	}
	if code, _ := do(t, "GET", srv.URL+"/v1/functions/nope", key, ""); code != 404 {
		t.Fatalf("get unknown = %d, want 404", code)
	}
	if code, _ := do(t, "DELETE", srv.URL+"/v1/functions/hello", key, ""); code != 200 {
		t.Fatalf("delete = %d", code)
	}
	if code, _ := do(t, "GET", srv.URL+"/v1/functions/hello", key, ""); code != 404 {
		t.Fatalf("get after delete = %d, want 404", code)
	}
}

func TestFunctionInvokeProxiesHandler(t *testing.T) {
	srv, key := newTestServer(t)

	handler := "import os\n" +
		"from http.server import HTTPServer,BaseHTTPRequestHandler\n" +
		"class H(BaseHTTPRequestHandler):\n" +
		"  def do_POST(s):\n" +
		"    n=int(s.headers.get('Content-Length',0)); body=s.rfile.read(n)\n" +
		"    s.send_response(200); s.end_headers(); s.wfile.write(b'echo:'+body)\n" +
		"  def do_GET(s): s.send_response(200); s.end_headers(); s.wfile.write(b'root')\n" +
		"HTTPServer(('127.0.0.1',int(os.environ['PORT'])),H).serve_forever()\n"

	code, out := do(t, "POST", srv.URL+"/v1/functions", key,
		`{"name":"echo","template":"python-3.11","start":"python3 handler.py","port":8000,
		  "files":[{"path":"handler.py","content":`+jsonString(handler)+`}]}`)
	if code != 201 {
		t.Fatalf("register = %d (%v)", code, out)
	}

	body, status, hdr := invoke(t, srv.URL+"/v1/functions/echo/invoke", key, `{"n":1}`)
	if status != 200 || body != `echo:{"n":1}` {
		t.Fatalf("invoke = %d %q", status, body)
	}
	if hdr.Get("X-Sandboxd-Cold-Start") != "true" {
		t.Fatalf("expected cold start header, got %q", hdr.Get("X-Sandboxd-Cold-Start"))
	}

	_, _, hdr2 := invoke(t, srv.URL+"/v1/functions/echo/invoke", key, `{}`)
	if hdr2.Get("X-Sandboxd-Cold-Start") != "false" {
		t.Fatalf("expected warm, got %q", hdr2.Get("X-Sandboxd-Cold-Start"))
	}

	_, status404, _ := invoke(t, srv.URL+"/v1/functions/nope/invoke", key, `{}`)
	if status404 != 404 {
		t.Fatalf("invoke unknown = %d, want 404", status404)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func invoke(t *testing.T, url, key, body string) (string, int, http.Header) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("invoke %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode, resp.Header
}

func TestFunctionCodeAndMetricsEndpoints(t *testing.T) {
	srv, key := newTestServer(t)

	handler := "import os\n" +
		"from http.server import HTTPServer,BaseHTTPRequestHandler\n" +
		"class H(BaseHTTPRequestHandler):\n" +
		"  def do_POST(s): s.send_response(200); s.end_headers(); s.wfile.write(b'ok')\n" +
		"HTTPServer(('127.0.0.1',int(os.environ['PORT'])),H).serve_forever()\n"
	code, out := do(t, "POST", srv.URL+"/v1/functions", key,
		`{"name":"m","template":"python-3.11","start":"python3 handler.py","port":8000,
		  "env":{"K":"v"},"files":[{"path":"handler.py","content":`+jsonString(handler)+`}]}`)
	if code != 201 {
		t.Fatalf("register = %d (%v)", code, out)
	}

	code, out = do(t, "GET", srv.URL+"/v1/functions/m/code", key, "")
	if code != 200 {
		t.Fatalf("code = %d (%v)", code, out)
	}
	files, _ := out["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("code files = %v", out["files"])
	}

	code, out = do(t, "GET", srv.URL+"/v1/functions/m/metrics", key, "")
	if code != 200 || out["invokes"] == nil || out["cold_starts"] == nil || out["state"] == nil {
		t.Fatalf("metrics = %d (%v)", code, out)
	}
	if out["state"] != "cold" {
		t.Fatalf("state before invoke = %v, want cold", out["state"])
	}

	invoke(t, srv.URL+"/v1/functions/m/invoke", key, `{}`)
	_, out = do(t, "GET", srv.URL+"/v1/functions/m/metrics", key, "")
	if int(out["invokes"].(float64)) != 1 || int(out["cold_starts"].(float64)) != 1 {
		t.Fatalf("metrics after invoke = %v", out)
	}

	if c, _ := do(t, "GET", srv.URL+"/v1/functions/nope/code", key, ""); c != 404 {
		t.Fatalf("code unknown = %d", c)
	}
	if c, _ := do(t, "GET", srv.URL+"/v1/functions/nope/metrics", key, ""); c != 404 {
		t.Fatalf("metrics unknown = %d", c)
	}
}

func TestFunctionResourceSizing(t *testing.T) {
	srv, key := newTestServer(t)
	code, out := do(t, "POST", srv.URL+"/v1/functions", key,
		`{"name":"big","template":"code-interpreter","start":"python3 handler.py","port":8000,"vcpu":4,"memory_mb":4096}`)
	if code != 201 {
		t.Fatalf("register = %d (%v)", code, out)
	}
	if v, _ := out["vcpu"].(float64); int(v) != 4 {
		t.Fatalf("vcpu = %v, want 4", out["vcpu"])
	}
	if m, _ := out["memory_mb"].(float64); int(m) != 4096 {
		t.Fatalf("memory_mb = %v, want 4096", out["memory_mb"])
	}
	// 0 falls back to the template default; code-interpreter is 2 vCPU / 1024 MB.
	code, out = do(t, "POST", srv.URL+"/v1/functions", key,
		`{"name":"def","template":"code-interpreter","start":"python3 handler.py","port":8000}`)
	if code != 201 {
		t.Fatalf("register default = %d (%v)", code, out)
	}
	if v, _ := out["vcpu"].(float64); int(v) != 2 {
		t.Fatalf("default vcpu = %v, want 2 (template)", out["vcpu"])
	}
}
