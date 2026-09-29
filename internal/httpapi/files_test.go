package httpapi

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestFileAPIRejectsPathTraversal(t *testing.T) {
	srv, key := newTestServer(t)
	_, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"python-3.11"}`)
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("create failed: %v", out)
	}
	for _, p := range []string{"../../etc/passwd", "a/../../b", "foo/../../../etc/shadow"} {
		req, _ := http.NewRequest("POST",
			srv.URL+"/v1/sandboxes/"+id+"/files?path="+url.QueryEscape(p), strings.NewReader("x"))
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("write %q: %v", p, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("write path=%q = %d, want 400", p, resp.StatusCode)
		}
	}
	if code, _ := do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/files?path=ok.txt", key, "hello"); code != 200 {
		t.Errorf("normal write = %d, want 200", code)
	}
}

// /files is exempt from MAX_REQUEST_BYTES and bounded by PD_FILES_MAX_BYTES,
// whose refusal names the limit; every other route keeps the general cap.
func TestWriteFileIsBoundedByTheFilesLimitOnly(t *testing.T) {
	t.Setenv("MAX_REQUEST_BYTES", "128")
	t.Setenv("PD_FILES_MAX_BYTES", "700")
	srv, key := newTestServer(t)
	_, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"python-3.11"}`)
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("create failed: %v", out)
	}
	post := func(path, body string) (int, string) {
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	// Over the general cap but under the files cap: accepted.
	if code, body := post("/v1/sandboxes/"+id+"/files?path=mid.bin", strings.Repeat("A", 500)); code != http.StatusOK {
		t.Fatalf("500-byte file under PD_FILES_MAX_BYTES = %d %s, want 200", code, body)
	}
	// Over the files cap: 413 naming it.
	code, body := post("/v1/sandboxes/"+id+"/files?path=big.bin", strings.Repeat("A", 701))
	if code != http.StatusRequestEntityTooLarge || !strings.Contains(body, "PD_FILES_MAX_BYTES") {
		t.Fatalf("oversize file = %d %s, want 413 naming PD_FILES_MAX_BYTES", code, body)
	}
	// Control: a JSON route still has the general cap (decodeBody reports the
	// truncated body as 400; either way the create must not succeed).
	if code, _ := post("/v1/sandboxes", `{"template":"python-3.11","start":"`+strings.Repeat("x", 500)+`"}`); code == http.StatusCreated {
		t.Fatalf("oversize create = %d, the general cap no longer applies", code)
	}
}

func TestFilesOverHTTP(t *testing.T) {
	srv, key := newTestServer(t)
	_, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"python-3.11"}`)
	id := out["id"].(string)

	req, _ := http.NewRequest("POST", srv.URL+"/v1/sandboxes/"+id+"/files?path=hello.txt",
		strings.NewReader("world"))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("write = %d", resp.StatusCode)
	}

	req, _ = http.NewRequest("GET", srv.URL+"/v1/sandboxes/"+id+"/files?path=hello.txt", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, _ = http.DefaultClient.Do(req)
	buf := make([]byte, 16)
	n, _ := resp.Body.Read(buf)
	if got := string(buf[:n]); got != "world" {
		t.Fatalf("read = %q, want world", got)
	}

	do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/filesystem/mkdir", key, `{"path":"sub"}`)
	code, out := do(t, "GET", srv.URL+"/v1/sandboxes/"+id+"/filesystem/list?path=.", key, "")
	if code != 200 || out["entries"] == nil {
		t.Fatalf("list = %d %v", code, out)
	}
	// Entries must use snake_case keys like the rest of the API.
	entries, _ := out["entries"].([]any)
	if len(entries) == 0 {
		t.Fatalf("no entries returned: %v", out)
	}
	first, _ := entries[0].(map[string]any)
	if _, ok := first["name"]; !ok {
		t.Fatalf("entry missing snake_case \"name\" key: %v", first)
	}
	if _, ok := first["is_dir"]; !ok {
		t.Fatalf("entry missing snake_case \"is_dir\" key: %v", first)
	}
}
