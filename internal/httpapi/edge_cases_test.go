package httpapi

import (
	"strings"
	"sync"
	"testing"
)

func TestCreateRejectsMalformedJSON(t *testing.T) {
	srv, key := newTestServer(t)
	if code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":`); code != 400 {
		t.Fatalf("malformed create = %d (%v), want 400", code, out)
	}
}

func TestCreateAcceptsEmptyBody(t *testing.T) {
	srv, key := newTestServer(t)
	if code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, ``); code != 201 {
		t.Fatalf("empty-body create = %d (%v), want 201", code, out)
	}
}

func TestExecRejectsMalformedJSON(t *testing.T) {
	srv, key := newTestServer(t)
	_, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"python-3.11"}`)
	id, _ := out["id"].(string)
	if code, _ := do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/exec", key, `{"cmd":`); code != 400 {
		t.Fatalf("malformed exec = %d, want 400", code)
	}
}

func TestCreateRejectsUnknownTemplate(t *testing.T) {
	srv, key := newTestServer(t)
	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"golang-999"}`)
	if code != 400 {
		t.Fatalf("unknown template = %d (%v), want 400", code, out)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "golang-999") {
		t.Fatalf("error = %q, want it to name the bad template", msg)
	}
	if code, _ := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"node-20"}`); code != 201 {
		t.Fatalf("known template = %d, want 201", code)
	}
}

func TestFilesystemErrorHidesHostPath(t *testing.T) {
	srv, key := newTestServer(t)
	_, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"python-3.11"}`)
	id, _ := out["id"].(string)

	code, body := getText(t, srv.URL+"/v1/sandboxes/"+id+"/files?path=app/missing.txt", key)
	if code != 404 {
		t.Fatalf("read missing = %d, want 404", code)
	}
	for _, leak := range []string{"/private", "/tmp", "/var", "sandboxes/sb_", t.TempDir()} {
		if strings.Contains(body, leak) {
			t.Fatalf("error leaks host path %q: %s", leak, body)
		}
	}
	if !strings.Contains(body, "/app/missing.txt") {
		t.Fatalf("error should reference the sandbox-relative path: %s", body)
	}
}

func TestActiveSandboxCap(t *testing.T) {
	t.Setenv("MAX_ACTIVE_SANDBOXES", "2")
	t.Setenv("RATE_LIMIT_RPS", "0")
	srv, key := newTestServer(t)

	for i := 0; i < 2; i++ {
		if code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"python-3.11"}`); code != 201 {
			t.Fatalf("create %d = %d (%v), want 201", i, code, out)
		}
	}
	if code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"python-3.11"}`); code != 429 {
		t.Fatalf("over-cap create = %d (%v), want 429", code, out)
	}
}

func TestRateLimitPerKey(t *testing.T) {
	t.Setenv("RATE_LIMIT_RPS", "1")
	t.Setenv("RATE_LIMIT_BURST", "1")
	t.Setenv("MAX_ACTIVE_SANDBOXES", "0")
	srv, key := newTestServer(t)

	var got429 bool
	for i := 0; i < 8; i++ {
		code, _ := getText(t, srv.URL+"/v1/sandboxes", key)
		if code == 429 {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Fatal("expected a 429 under a 1 rps / burst 1 limit")
	}
}

func TestRateLimitDisabledAllowsBurst(t *testing.T) {
	t.Setenv("RATE_LIMIT_RPS", "0")
	srv, key := newTestServer(t)
	var wg sync.WaitGroup
	codes := make([]int, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _ = getText(t, srv.URL+"/v1/sandboxes", key)
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != 200 {
			t.Fatalf("req %d = %d, want 200 (limiter disabled)", i, c)
		}
	}
}
