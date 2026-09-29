package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/httpapi"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/process"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// smokeTestPreviewSecret is set directly because this test builds the
// httpapi.Server without nodeboot, and preview is fail-closed without a secret.
const smokeTestPreviewSecret = "smoke-test-preview-secret"

// TestSmokeOneCallDeployAndPreview deploys a $PORT-honoring app in one call
// and fetches its preview URL through the proxy.
func TestSmokeOneCallDeployAndPreview(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.Open(t.Context(), filepath.Join(dir, "s.db"))
	defer st.Close()
	_, key, _ := st.CreateKey(t.Context(), "smoke", "default")
	d := daemon.New(daemon.Config{
		Runtime:     process.New(filepath.Join(dir, "sb")),
		Store:       daemon.StoreAdapter{S: st},
		SnapshotDir: filepath.Join(dir, "snap"),
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	// The deployed app otherwise outlives the test.
	t.Cleanup(func() { d.Shutdown(context.Background()) })
	api := httpapi.New(d, st)
	api.SetPreviewSecret([]byte(smokeTestPreviewSecret))
	srv := httptest.NewServer(api.Routes())
	defer srv.Close()

	app := "import os\n" +
		"from http.server import HTTPServer,BaseHTTPRequestHandler\n" +
		"class H(BaseHTTPRequestHandler):\n" +
		"  def do_GET(s):\n" +
		"    s.send_response(200); s.end_headers(); s.wfile.write(b'live')\n" +
		"HTTPServer(('127.0.0.1',int(os.environ['PORT'])),H).serve_forever()\n"
	body, _ := json.Marshal(map[string]any{
		"template": "python-3.11",
		"files":    []map[string]string{{"path": "app.py", "content": app}},
		"start":    "python3 app.py",
		"port":     8000,
	})
	req, _ := http.NewRequest("POST", srv.URL+"/v1/sandboxes", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	if resp.StatusCode != 201 {
		t.Fatalf("create: code=%d", resp.StatusCode)
	}
	var created map[string]any
	json.NewDecoder(resp.Body).Decode(&created)
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("create response has no id: %v", created)
	}
	// app_url already carries the org-bound ?ptok= the preview gate requires.
	previewURL, _ := created["app_url"].(string)
	if previewURL == "" {
		t.Fatalf("create response has no app_url (expected a ?ptok=-bearing preview link): %v", created)
	}

	var got string
	for i := 0; i < 50; i++ {
		r, err := http.Get(previewURL)
		if err == nil {
			b, _ := io.ReadAll(r.Body)
			r.Body.Close()
			got = string(b)
			if got == "live" {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got != "live" {
		t.Fatalf("preview body = %q, want live", got)
	}
}
