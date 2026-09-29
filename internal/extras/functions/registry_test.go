package functions

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/runtime/process"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	d := daemon.New(daemon.Config{
		Runtime:     process.New(filepath.Join(dir, "sb")),
		Store:       daemon.StoreAdapter{S: st},
		SnapshotDir: filepath.Join(dir, "snap"),
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	return NewRegistry(d, st, filepath.Join(dir, "functions"))
}

func TestRegisterGetListDelete(t *testing.T) {
	r := newTestRegistry(t)
	fn, err := r.Register(t.Context(), "default", RegisterSpec{
		Name: "hello", Template: "python-3.11", Start: "python3 handler.py", Port: 8000,
		Files: []FileSpec{{Path: "handler.py", Content: "print('hi')"}},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if fn.Name != "hello" || fn.Port != 8000 {
		t.Fatalf("got %+v", fn)
	}
	if _, err := r.Register(t.Context(), "default", RegisterSpec{Name: "hello", Template: "python-3.11"}); err != ErrExists {
		t.Fatalf("dup register = %v, want ErrExists", err)
	}
	got, ok, err := r.Get(t.Context(), "default", "hello")
	if err != nil || !ok || got.Template != "python-3.11" {
		t.Fatalf("get: %v ok=%v %+v", err, ok, got)
	}
	if _, ok, _ := r.Get(t.Context(), "acme", "hello"); ok {
		t.Fatalf("cross-org get should miss")
	}
	list, err := r.List(t.Context(), "default")
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v len=%d", err, len(list))
	}
	code := filepath.Join(r.codeDir, "default", "hello", "handler.py")
	if b, err := os.ReadFile(code); err != nil || string(b) != "print('hi')" {
		t.Fatalf("code file: %v %q", err, b)
	}
	if err := r.Delete(t.Context(), "default", "hello"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok, _ := r.Get(t.Context(), "default", "hello"); ok {
		t.Fatalf("still present after delete")
	}
}

func TestRegisterRejectsUnsafeNames(t *testing.T) {
	r := newTestRegistry(t)
	for _, bad := range []string{"", "..", "../evil", "a/b", "foo/../bar", ".", "-x", "a.b"} {
		if _, err := r.Register(t.Context(), "default", RegisterSpec{Name: bad, Template: "python-3.11"}); err != ErrInvalidName {
			t.Errorf("Register(%q) err = %v, want ErrInvalidName", bad, err)
		}
	}
	if _, err := r.Register(t.Context(), "default", RegisterSpec{Name: "hello-world_2", Template: "python-3.11"}); err != nil {
		t.Fatalf("valid name rejected: %v", err)
	}
}

func TestEnsureSandboxLifecycle(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	handler := "import os\n" +
		"from http.server import HTTPServer,BaseHTTPRequestHandler\n" +
		"class H(BaseHTTPRequestHandler):\n" +
		"  def do_GET(s): s.send_response(200); s.end_headers(); s.wfile.write(b'ok')\n" +
		"HTTPServer(('127.0.0.1',int(os.environ['PORT'])),H).serve_forever()\n"
	if _, err := r.Register(t.Context(), "default", RegisterSpec{
		Name: "f", Template: "python-3.11", Start: "python3 handler.py", Port: 8000,
		Files: []FileSpec{{Path: "handler.py", Content: handler}},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	id1, cold1, err := r.EnsureSandbox(ctx, "default", "f")
	if err != nil || id1 == "" || !cold1 {
		t.Fatalf("ensure #1: id=%q cold=%v err=%v", id1, cold1, err)
	}
	id2, cold2, err := r.EnsureSandbox(ctx, "default", "f")
	if err != nil || id2 != id1 || cold2 {
		t.Fatalf("ensure #2: id=%q cold=%v err=%v", id2, cold2, err)
	}
	if _, _, err := r.EnsureSandbox(ctx, "default", "nope"); err != ErrNotFound {
		t.Fatalf("ensure unknown = %v, want ErrNotFound", err)
	}
	// Update destroys the sandbox so the next ensure is cold again.
	if err := r.Update(t.Context(), "default", "f", RegisterSpec{
		Template: "python-3.11", Start: "python3 handler.py", Port: 8000,
		Files: []FileSpec{{Path: "handler.py", Content: handler}},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	id3, cold3, err := r.EnsureSandbox(ctx, "default", "f")
	if err != nil || !cold3 || id3 == id1 {
		t.Fatalf("ensure after update: id=%q cold=%v err=%v", id3, cold3, err)
	}
}

func TestEnvRoundTripAndCode(t *testing.T) {
	r := newTestRegistry(t)
	fn, err := r.Register(t.Context(), "default", RegisterSpec{
		Name: "f", Template: "python-3.11", Start: "python3 handler.py", Port: 8000,
		Env:   map[string]string{"API_KEY": "s3cret"},
		Files: []FileSpec{{Path: "handler.py", Content: "print('hi')"}},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if fn.Env["API_KEY"] != "s3cret" {
		t.Fatalf("register env = %v", fn.Env)
	}
	got, _, _ := r.Get(t.Context(), "default", "f")
	if got.Env["API_KEY"] != "s3cret" {
		t.Fatalf("get env = %v", got.Env)
	}
	files, err := r.Code(t.Context(), "default", "f")
	if err != nil || len(files) != 1 || files[0].Path != "handler.py" || files[0].Content != "print('hi')" {
		t.Fatalf("code = %v err=%v", files, err)
	}
	if _, err := r.Code(t.Context(), "default", "nope"); err != ErrNotFound {
		t.Fatalf("code unknown = %v, want ErrNotFound", err)
	}
}

func TestUpdatePreservesCodeWhenNoFiles(t *testing.T) {
	r := newTestRegistry(t)
	if _, err := r.Register(t.Context(), "default", RegisterSpec{
		Name: "f", Template: "python-3.11", Start: "python3 handler.py", Port: 8000,
		Files: []FileSpec{{Path: "handler.py", Content: "ORIGINAL"}},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := r.Update(t.Context(), "default", "f", RegisterSpec{
		Template: "python-3.11", Start: "python3 handler.py", Port: 9000,
		Env: map[string]string{"LOG": "debug"},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	files, _ := r.Code(t.Context(), "default", "f")
	if len(files) != 1 || files[0].Content != "ORIGINAL" {
		t.Fatalf("code not preserved: %v", files)
	}
	got, _, _ := r.Get(t.Context(), "default", "f")
	if got.Port != 9000 || got.Env["LOG"] != "debug" {
		t.Fatalf("settings not updated: %+v", got)
	}
	if err := r.Update(t.Context(), "default", "f", RegisterSpec{
		Template: "python-3.11", Start: "python3 handler.py", Port: 9000,
		Files: []FileSpec{{Path: "handler.py", Content: "NEW"}},
	}); err != nil {
		t.Fatalf("update2: %v", err)
	}
	files, _ = r.Code(t.Context(), "default", "f")
	if files[0].Content != "NEW" {
		t.Fatalf("code not rewritten: %v", files)
	}
}

func TestRecordInvokeCounts(t *testing.T) {
	r := newTestRegistry(t)
	r.Register(t.Context(), "default", RegisterSpec{Name: "f", Template: "python-3.11"})
	r.RecordInvoke(t.Context(), "default", "f", true)
	r.RecordInvoke(t.Context(), "default", "f", false)
	got, _, _ := r.Get(t.Context(), "default", "f")
	if got.Invokes != 2 || got.ColdStarts != 1 {
		t.Fatalf("invokes=%d cold=%d, want 2/1", got.Invokes, got.ColdStarts)
	}
}
