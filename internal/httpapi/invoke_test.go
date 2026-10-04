package httpapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/entitlements"
	"github.com/pilldrops-oss/pilldrops/internal/extras/functions"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/process"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// oneSandboxPlan installs a catalogue whose only plan allows one concurrent
// sandbox; the shipped plans start above the autoscaler's default burst.
func oneSandboxPlan(t *testing.T) entitlements.Plan {
	t.Helper()
	p := entitlements.Plan{
		ID: "solo", Name: "Solo", MaxConcurrent: 1, SessionTTLHrs: 4,
		MaxVCPU: 2, MaxMemMB: 2048, MaxStorageMB: 2048, IncludedHours: 10,
	}
	entitlements.SetCatalog([]entitlements.Plan{p})
	t.Cleanup(entitlements.Reset)
	return p
}

// newBilledFunctionServer is the billed test server with the function registry wired.
func newBilledFunctionServer(t *testing.T) (*httptest.Server, *daemon.Daemon, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	_, key, err := st.CreateKey(t.Context(), "test", "default")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	d := daemon.New(daemon.Config{
		Runtime:     process.New(filepath.Join(dir, "sb")),
		Store:       daemon.StoreAdapter{S: st},
		SnapshotDir: filepath.Join(dir, "snap"),
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	s := New(d, st)
	s.SetFunctions(functions.NewRegistry(d, st, filepath.Join(dir, "functions")))
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	// The first plan in the current catalogue, not a hardcoded id: a test that
	// installed its own catalogue would otherwise fall back to unlimited.
	if err := st.SetPlan(t.Context(), "default", entitlements.Plans()[0].ID); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}
	return srv, d, key
}

// An invoke reaches daemon.Create without the create handler's cap, so it must
// enforce the plan's concurrency limit itself.
func TestInvokeCannotCreateSandboxesPastThePlanCap(t *testing.T) {
	plan := oneSandboxPlan(t)
	srv, d, key := newBilledFunctionServer(t)

	if code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base"}`); code != http.StatusCreated {
		t.Fatalf("first create = %d (%v)", code, out)
	}
	if code, _ := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base"}`); code != http.StatusTooManyRequests {
		t.Fatalf("second create = %d, want 429 — the create-path cap is not in force, so this test proves nothing", code)
	}

	if code, out := do(t, "POST", srv.URL+"/v1/functions", key,
		`{"name":"sneaky","template":"base","start":"sleep 600","port":8000}`); code != http.StatusCreated {
		t.Fatalf("register = %d (%v)", code, out)
	}

	before := len(d.List("default"))
	body, status, _ := invoke(t, srv.URL+"/v1/functions/sneaky/invoke", key, `{}`)
	if status != http.StatusTooManyRequests {
		t.Fatalf("invoke at the cap = %d (%q), want 429 — the function path is still creating sandboxes outside every limit",
			status, body)
	}
	if !strings.Contains(body, plan.ID) {
		t.Errorf("refusal %q does not name the plan the customer is on", body)
	}
	if after := len(d.List("default")); after != before {
		t.Errorf("sandboxes went %d → %d while refusing the invoke — a machine was created anyway", before, after)
	}
}

// The cap rations machines, not requests: a warm replica still serves at the cap.
func TestInvokeAtTheCapStillServesAWarmReplica(t *testing.T) {
	oneSandboxPlan(t)
	srv, d, key := newBilledFunctionServer(t)

	handler := "import os\n" +
		"from http.server import HTTPServer,BaseHTTPRequestHandler\n" +
		"class H(BaseHTTPRequestHandler):\n" +
		"  def do_GET(s): s.send_response(200); s.end_headers(); s.wfile.write(b'warm')\n" +
		"  def do_POST(s): s.send_response(200); s.end_headers(); s.wfile.write(b'warm')\n" +
		"HTTPServer(('127.0.0.1',int(os.environ['PORT'])),H).serve_forever()\n"
	if code, out := do(t, "POST", srv.URL+"/v1/functions", key,
		`{"name":"warm","template":"python-3.11","start":"python3 handler.py","port":8000,
		  "files":[{"path":"handler.py","content":`+jsonString(handler)+`}]}`); code != http.StatusCreated {
		t.Fatalf("register = %d (%v)", code, out)
	}

	if body, status, _ := invoke(t, srv.URL+"/v1/functions/warm/invoke", key, `{}`); status != http.StatusOK {
		t.Fatalf("cold invoke = %d (%q)", status, body)
	}
	if n := len(d.List("default")); n != 1 {
		t.Fatalf("cold start produced %d sandboxes, want 1", n)
	}

	if body, status, _ := invoke(t, srv.URL+"/v1/functions/warm/invoke", key, `{}`); status != http.StatusOK {
		t.Errorf("warm invoke = %d (%q), want 200 — the cap is refusing requests a running replica can serve",
			status, body)
	}
}

// An unset max_replicas is clamped to the plan, not left at the autoscaler default.
func TestUnsetMaxReplicasIsStillClampedToThePlan(t *testing.T) {
	plan := oneSandboxPlan(t)
	srv, _, key := newBilledFunctionServer(t)

	if code, out := do(t, "POST", srv.URL+"/v1/functions", key,
		`{"name":"unbounded","template":"base","start":"sleep 600","port":8000}`); code != http.StatusCreated {
		t.Fatalf("register = %d (%v)", code, out)
	}
	code, out := do(t, "GET", srv.URL+"/v1/functions/unbounded", key, "")
	if code != http.StatusOK {
		t.Fatalf("get = %d (%v)", code, out)
	}
	got, _ := out["max_replicas"].(float64)
	if int(got) != plan.MaxConcurrent {
		t.Errorf("max_replicas = %v on a plan allowing %d concurrent sandboxes — autoscaling can still outgrow the plan",
			out["max_replicas"], plan.MaxConcurrent)
	}

	if code, out := do(t, "POST", srv.URL+"/v1/functions", key,
		`{"name":"greedy","template":"base","start":"sleep 600","port":8000,"max_replicas":50}`); code != http.StatusCreated {
		t.Fatalf("register greedy = %d (%v)", code, out)
	}
	_, out = do(t, "GET", srv.URL+"/v1/functions/greedy", key, "")
	if got, _ := out["max_replicas"].(float64); int(got) != plan.MaxConcurrent {
		t.Errorf("explicit max_replicas = %v, want the plan's %d", out["max_replicas"], plan.MaxConcurrent)
	}
}
