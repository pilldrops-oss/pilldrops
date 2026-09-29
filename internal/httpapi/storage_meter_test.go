package httpapi

import (
	"bytes"
	"net/http"
	"testing"
)

func TestSandboxGetReportsStorage(t *testing.T) {
	srv, key, sb, _ := newCronSandbox(t)

	blob := bytes.Repeat([]byte("x"), 3<<20)
	req, _ := http.NewRequest("POST", srv+"/v1/sandboxes/"+sb+"/files?path=big.bin", bytes.NewReader(blob))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("write blob: %v %v", err, resp.StatusCode)
	}
	resp.Body.Close()

	code, body := do(t, "GET", srv+"/v1/sandboxes/"+sb, key, "")
	if code != http.StatusOK {
		t.Fatalf("get = %d", code)
	}
	used, ok := body["storage_used_mb"].(float64)
	if !ok {
		t.Fatalf("storage_used_mb missing from sandbox GET: %v", body)
	}
	if used < 3 {
		t.Fatalf("storage_used_mb = %v, want >= 3 after writing 3MB", used)
	}
	if _, ok := body["storage_mb"].(float64); !ok {
		t.Fatalf("storage_mb (the cap) missing: %v", body)
	}
}

func TestBillingReportsStorageRollup(t *testing.T) {
	srv, key, _, _ := newCronSandbox(t)
	code, body := do(t, "GET", srv+"/v1/limits", key, "")
	if code != http.StatusOK {
		t.Fatalf("limits = %d", code)
	}
	st, ok := body["storage"].(map[string]any)
	if !ok {
		t.Fatalf("billing has no storage rollup: %v", body)
	}
	for _, k := range []string{"used_mb", "cap_mb", "sandboxes", "measured"} {
		if _, present := st[k]; !present {
			t.Errorf("storage rollup missing %q: %v", k, st)
		}
	}
	if n, _ := st["sandboxes"].(float64); n < 1 {
		t.Errorf("storage rollup counted %v sandboxes, want >= 1", n)
	}
	// measured lets the UI distinguish "0 MB" from "cannot tell".
	if m, _ := st["measured"].(bool); !m {
		t.Errorf("measured = false on a runtime that implements DiskUsager")
	}
}
