package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

func createWithIdemKey(t *testing.T, url, apiKey, body, idem string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idem)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

// doWithIdemKey is do() with an Idempotency-Key header when idem is non-empty.
func doWithIdemKey(t *testing.T, method, url, apiKey, body, idem string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

func createSandbox(t *testing.T, url, apiKey string) string {
	t.Helper()
	code, out := do(t, "POST", url+"/v1/sandboxes", apiKey, `{"template":"python-3.11"}`)
	if code != 201 {
		t.Fatalf("create = %d (%v)", code, out)
	}
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("no id in %v", out)
	}
	return id
}

// A retried DELETE with the same key replays the first response instead of 404ing.
func TestIdempotentDestroy(t *testing.T) {
	srv, key := newTestServer(t)
	id := createSandbox(t, srv.URL, key)

	code1, out1 := doWithIdemKey(t, "DELETE", srv.URL+"/v1/sandboxes/"+id, key, "", "idem-destroy-1")
	if code1 != 200 || out1["status"] != "destroyed" {
		t.Fatalf("first destroy = %d %v", code1, out1)
	}

	code2, out2 := doWithIdemKey(t, "DELETE", srv.URL+"/v1/sandboxes/"+id, key, "", "idem-destroy-1")
	if code2 != 200 || out2["status"] != "destroyed" {
		t.Fatalf("replayed destroy = %d %v, want 200 {status:destroyed}", code2, out2)
	}
}

// countAuditByAction counts a sandbox's audit entries for one action. Pause and
// resume are daemon-level no-ops on a retry, so only the audit count shows
// whether the replay path ran.
func countAuditByAction(t *testing.T, st *store.Store, target, action string) int {
	t.Helper()
	entries, err := st.ListAuditByTarget(t.Context(), target, 10)
	if err != nil {
		t.Fatalf("ListAuditByTarget(%s): %v", target, err)
	}
	n := 0
	for _, e := range entries {
		if e.Action == action {
			n++
		}
	}
	return n
}

func TestIdempotentPause(t *testing.T) {
	srv, st, key := newTestServerWithStore(t)
	id := createSandbox(t, srv.URL, key)

	code1, out1 := doWithIdemKey(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/pause", key, "", "idem-pause-1")
	if code1 != 200 || out1["status"] != "paused" {
		t.Fatalf("first pause = %d %v", code1, out1)
	}

	code2, out2 := doWithIdemKey(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/pause", key, "", "idem-pause-1")
	if code2 != 200 || out2["status"] != "paused" {
		t.Fatalf("replayed pause = %d %v, want 200 {status:paused}", code2, out2)
	}

	if n := countAuditByAction(t, st, id, "sandbox.pause"); n != 1 {
		t.Fatalf("sandbox.pause audit entries after two identical-key calls = %d, want 1 (replay must short-circuit before the audit write)", n)
	}
}

func TestIdempotentResume(t *testing.T) {
	srv, st, key := newTestServerWithStore(t)
	id := createSandbox(t, srv.URL, key)

	if code, out := do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/pause", key, ""); code != 200 {
		t.Fatalf("setup pause = %d %v", code, out)
	}

	code1, out1 := doWithIdemKey(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/resume", key, "", "idem-resume-1")
	if code1 != 200 || out1["status"] != "running" {
		t.Fatalf("first resume = %d %v", code1, out1)
	}

	code2, out2 := doWithIdemKey(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/resume", key, "", "idem-resume-1")
	if code2 != 200 || out2["status"] != "running" {
		t.Fatalf("replayed resume = %d %v, want 200 {status:running}", code2, out2)
	}

	if n := countAuditByAction(t, st, id, "sandbox.resume"); n != 1 {
		t.Fatalf("sandbox.resume audit entries after two identical-key calls = %d, want 1 (replay must short-circuit before the audit write)", n)
	}
}

// A key reused across a batch must not replay A's response against B.
func TestIdempotencyKeyScopedToSandboxID_Destroy(t *testing.T) {
	srv, key := newTestServer(t)
	idA := createSandbox(t, srv.URL, key)
	idB := createSandbox(t, srv.URL, key)
	const sharedKey = "batch-destroy-key"

	if code, out := doWithIdemKey(t, "DELETE", srv.URL+"/v1/sandboxes/"+idA, key, "", sharedKey); code != 200 || out["status"] != "destroyed" {
		t.Fatalf("destroy A = %d %v", code, out)
	}

	code, out := doWithIdemKey(t, "DELETE", srv.URL+"/v1/sandboxes/"+idB, key, "", sharedKey)
	if code != 200 || out["status"] != "destroyed" {
		t.Fatalf("destroy B (reused key) = %d %v, want 200 {status:destroyed}", code, out)
	}
	if getCode, _ := do(t, "GET", srv.URL+"/v1/sandboxes/"+idB, key, ""); getCode != 404 {
		t.Fatalf("sandbox B still exists after reused-key destroy (GET = %d, want 404) — key reuse across ids falsely replayed A's response instead of destroying B", getCode)
	}
}

func TestIdempotencyKeyScopedToSandboxID_Pause(t *testing.T) {
	srv, key := newTestServer(t)
	idA := createSandbox(t, srv.URL, key)
	idB := createSandbox(t, srv.URL, key)
	const sharedKey = "batch-pause-key"

	if code, out := doWithIdemKey(t, "POST", srv.URL+"/v1/sandboxes/"+idA+"/pause", key, "", sharedKey); code != 200 || out["status"] != "paused" {
		t.Fatalf("pause A = %d %v", code, out)
	}

	code, out := doWithIdemKey(t, "POST", srv.URL+"/v1/sandboxes/"+idB+"/pause", key, "", sharedKey)
	if code != 200 || out["status"] != "paused" {
		t.Fatalf("pause B (reused key) = %d %v, want 200 {status:paused}", code, out)
	}
	_, getB := do(t, "GET", srv.URL+"/v1/sandboxes/"+idB, key, "")
	if getB["status"] != "paused" {
		t.Fatalf("sandbox B status = %v after reused-key pause, want \"paused\" — key reuse across ids falsely replayed A's response instead of pausing B", getB["status"])
	}
}

func TestIdempotencyKeyScopedToSandboxID_Resume(t *testing.T) {
	srv, key := newTestServer(t)
	idA := createSandbox(t, srv.URL, key)
	idB := createSandbox(t, srv.URL, key)
	if code, out := do(t, "POST", srv.URL+"/v1/sandboxes/"+idA+"/pause", key, ""); code != 200 {
		t.Fatalf("setup pause A = %d %v", code, out)
	}
	if code, out := do(t, "POST", srv.URL+"/v1/sandboxes/"+idB+"/pause", key, ""); code != 200 {
		t.Fatalf("setup pause B = %d %v", code, out)
	}
	const sharedKey = "batch-resume-key"

	if code, out := doWithIdemKey(t, "POST", srv.URL+"/v1/sandboxes/"+idA+"/resume", key, "", sharedKey); code != 200 || out["status"] != "running" {
		t.Fatalf("resume A = %d %v", code, out)
	}

	code, out := doWithIdemKey(t, "POST", srv.URL+"/v1/sandboxes/"+idB+"/resume", key, "", sharedKey)
	if code != 200 || out["status"] != "running" {
		t.Fatalf("resume B (reused key) = %d %v, want 200 {status:running}", code, out)
	}
	_, getB := do(t, "GET", srv.URL+"/v1/sandboxes/"+idB, key, "")
	if getB["status"] != "running" {
		t.Fatalf("sandbox B status = %v after reused-key resume, want \"running\" — key reuse across ids falsely replayed A's response instead of resuming B", getB["status"])
	}
}

// A key reused for pause then destroy on the same sandbox must not replay
// pause's response against the destroy.
func TestIdempotencyKeyScopedToAction_PauseThenDestroy(t *testing.T) {
	srv, key := newTestServer(t)
	id := createSandbox(t, srv.URL, key)
	const sharedKey = "cross-action-key"

	codeP, outP := doWithIdemKey(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/pause", key, "", sharedKey)
	if codeP != 200 || outP["status"] != "paused" {
		t.Fatalf("pause = %d %v", codeP, outP)
	}

	codeD, outD := doWithIdemKey(t, "DELETE", srv.URL+"/v1/sandboxes/"+id, key, "", sharedKey)
	if codeD != 200 || outD["status"] != "destroyed" {
		t.Fatalf("destroy (reused key from pause) = %d %v, want 200 {status:destroyed} — got a replayed pause response instead of a real destroy", codeD, outD)
	}

	if getCode, _ := do(t, "GET", srv.URL+"/v1/sandboxes/"+id, key, ""); getCode != 404 {
		t.Fatalf("sandbox still exists after reused-key destroy (GET = %d, want 404) — key reuse across actions falsely replayed pause's response instead of destroying the sandbox", getCode)
	}
}

// Without a key, destroying an already-gone sandbox still 404s.
func TestDestroyWithoutIdemKeyUnchanged(t *testing.T) {
	srv, key := newTestServer(t)
	id := createSandbox(t, srv.URL, key)

	if code, out := do(t, "DELETE", srv.URL+"/v1/sandboxes/"+id, key, ""); code != 200 {
		t.Fatalf("first destroy = %d %v", code, out)
	}
	if code, out := do(t, "DELETE", srv.URL+"/v1/sandboxes/"+id, key, ""); code != 404 {
		t.Fatalf("second destroy without idem key = %d %v, want 404 (unchanged behavior)", code, out)
	}
}

func TestCreateIsIdempotent(t *testing.T) {
	srv, key := newTestServer(t)
	body := `{"template":"python-3.11"}`

	_, out1 := createWithIdemKey(t, srv.URL+"/v1/sandboxes", key, body, "idem-abc")
	id1, _ := out1["id"].(string)
	if id1 == "" {
		t.Fatalf("first create failed: %v", out1)
	}

	_, out2 := createWithIdemKey(t, srv.URL+"/v1/sandboxes", key, body, "idem-abc")
	id2, _ := out2["id"].(string)
	if id1 != id2 {
		t.Fatalf("same Idempotency-Key returned different sandboxes: %s vs %s", id1, id2)
	}

	_, list := do(t, "GET", srv.URL+"/v1/sandboxes", key, "")
	sbs, _ := list["sandboxes"].([]any)
	if len(sbs) != 1 {
		t.Fatalf("idempotent create made %d sandboxes, want 1", len(sbs))
	}
}
