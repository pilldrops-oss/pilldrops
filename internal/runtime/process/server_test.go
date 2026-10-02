package process

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

func TestStartServerAndListenTarget(t *testing.T) {
	r := New(t.TempDir())
	ctx := context.Background()
	inst, err := r.Boot(ctx, rt.BootSpec{SandboxID: "sb1"})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}

	err = inst.FS().Write(t.Context(), "app.py", []byte(
		"import os\n"+
			"from http.server import HTTPServer,BaseHTTPRequestHandler\n"+
			"class H(BaseHTTPRequestHandler):\n"+
			"  def do_GET(s):\n"+
			"    s.send_response(200); s.end_headers(); s.wfile.write(b'hello-preview')\n"+
			"HTTPServer(('127.0.0.1',int(os.environ['PORT'])),H).serve_forever()\n"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := inst.StartServer(ctx, "python3 app.py", nil); err != nil {
		t.Fatalf("start server: %v", err)
	}

	target, err := inst.ListenTarget(8000)
	if err != nil || target == "" {
		t.Fatalf("listen target = %q, %v", target, err)
	}

	// Deadlines, not fixed waits: python's start and its exit after Stop both
	// stretch under -race and a loaded machine.
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	get := func() (string, error) {
		resp, err := client.Get("http://" + target + "/")
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		return string(b), err
	}
	var body string
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if body, err = get(); err == nil {
			break
		}
	}
	if body != "hello-preview" {
		t.Fatalf("preview body = %q (%v), want hello-preview", body, err)
	}

	if err := inst.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if _, err := get(); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server still responding 10s after Stop")
		}
	}
}

// Plain exec sees $PORT for parity with the start command; an explicit PORT wins.
func TestExecSeesAppPort(t *testing.T) {
	r := New(t.TempDir())
	inst, err := r.Boot(context.Background(), rt.BootSpec{SandboxID: "sb-port"})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	defer inst.Stop(context.Background())

	target, err := inst.ListenTarget(0)
	if err != nil {
		t.Fatalf("listen target: %v", err)
	}
	wantPort := target[len("127.0.0.1:"):]

	res, err := inst.Exec(context.Background(), rt.ExecSpec{Cmd: "printf %s \"$PORT\""})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.Stdout != wantPort {
		t.Fatalf("exec $PORT = %q, want %q (the sandbox's app port)", res.Stdout, wantPort)
	}

	res, err = inst.Exec(context.Background(), rt.ExecSpec{Cmd: "printf %s \"$PORT\"", Env: map[string]string{"PORT": "7777"}})
	if err != nil {
		t.Fatalf("exec override: %v", err)
	}
	if res.Stdout != "7777" {
		t.Fatalf("exec explicit PORT = %q, want 7777 (caller env wins)", res.Stdout)
	}
}
