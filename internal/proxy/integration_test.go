package proxy_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/proxy"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/process"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
)

// TestPreviewEndToEnd runs the real daemon and process runtime with a real app
// server, then proves a preview request wakes a paused sandbox. Needs python3.
func TestPreviewEndToEnd(t *testing.T) {
	dir := t.TempDir()
	d := daemon.New(daemon.Config{
		Runtime:     process.New(filepath.Join(dir, "sb")),
		SnapshotDir: filepath.Join(dir, "snap"),
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	ctx := context.Background()
	t.Cleanup(func() { d.Shutdown(ctx) })

	sb, err := d.Create(ctx, daemon.CreateSpec{Template: "python"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	fs, _ := d.FS(sb.ID)
	_ = fs.Write(t.Context(), "app.py", []byte(
		"import os\n"+
			"from http.server import HTTPServer,BaseHTTPRequestHandler\n"+
			"class H(BaseHTTPRequestHandler):\n"+
			"  def do_GET(s):\n"+
			"    s.send_response(200); s.end_headers(); s.wfile.write(b'served:'+s.path.encode())\n"+
			"HTTPServer(('127.0.0.1',int(os.environ['PORT'])),H).serve_forever()\n"))
	if err := d.StartServer(ctx, sb.ID, "python3 app.py", 8000, nil); err != nil {
		t.Fatalf("start server: %v", err)
	}

	// An ungated mount must resolve nothing. Checked against the same sandbox
	// the gated mount serves below, so "refused" is not the app being down.
	bare := httptest.NewServer(proxy.Handler(d))
	defer bare.Close()
	if resp, err := http.Get(bare.URL + "/preview/" + sb.ID + "/8000/hi"); err == nil {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("ungated /preview mount answered %d for a real sandbox; it must refuse "+
				"— nothing on that path established who is asking", resp.StatusCode)
		}
	} else {
		t.Fatalf("ungated probe never reached the server (%v) — a refusal from a request "+
			"that did not arrive proves nothing", err)
	}

	gated := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy.Handler(d).ServeHTTP(w, r.WithContext(proxy.WithPreviewOrg(r.Context(), sb.OrgID)))
	})
	srv := httptest.NewServer(gated)
	defer srv.Close()

	get := func(path string) (int, string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			return 0, ""
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}
	var code int
	var body string
	for i := 0; i < 50; i++ {
		code, body = get("/preview/" + sb.ID + "/8000/hi")
		if code == 200 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if code != 200 || body != "served:/hi" {
		t.Fatalf("preview = %d %q, want 200 served:/hi", code, body)
	}

	if err := d.Pause(ctx, sb.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, err := d.PreviewTarget(ctx, sb.ID, 8000, sb.OrgID); err != nil {
		t.Fatalf("wake: %v", err)
	}
	if got, _ := d.Get(sb.ID); got.State != daemon.StateRunning {
		t.Fatalf("sandbox not awake after preview target; state=%s", got.State)
	}
}
