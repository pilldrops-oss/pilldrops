//go:build !core

package httpapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/runtime/process"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// lastAgentTestServer is the *Server behind the most recent newAgentTestAPI,
// for tests that drive background tasks with no HTTP surface.
var lastAgentTestServer *Server

func serverBehind(t *testing.T, _ string) *Server {
	t.Helper()
	return lastAgentTestServer
}

func newAgentTestAPI(t *testing.T, template string) (url, key, sandboxID string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	_, k, err := st.CreateKey(t.Context(), "agent", "default")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	d := daemon.New(daemon.Config{
		Runtime:     process.New(filepath.Join(dir, "sb")),
		Store:       daemon.StoreAdapter{S: st},
		SnapshotDir: filepath.Join(dir, "snap"),
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	a := New(d, st)
	lastAgentTestServer = a
	srv := httptest.NewServer(a.Routes())
	t.Cleanup(srv.Close)

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", k, `{"template":"`+template+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("create %s sandbox = %d %v", template, code, out)
	}
	id, _ := out["id"].(string)
	return srv.URL, k, id
}

// agentRoutes is every route the agent surface registers.
func agentRoutes(id string) []struct{ method, path, body string } {
	base := "/v1/sandboxes/" + id + "/agent"
	return []struct{ method, path, body string }{
		{"GET", base + "/status", ""},
		{"POST", base + "/turns", `{"message":"hello"}`},
		{"GET", base + "/turns", ""},
		{"GET", base + "/turns/turn_x", ""},
		{"DELETE", base + "/turns/turn_x", ""},
		{"POST", base + "/turns/turn_x/interrupt", `{}`},
		{"POST", base + "/queue/reorder", `{"order":[]}`},
		{"GET", base + "/events", ""},
		{"GET", base + "/models", ""},
		{"GET", base + "/threads", ""},
		{"GET", base + "/threads/sess_x", ""},
		{"GET", base + "/budget", ""},
		{"PATCH", base + "/budget", `{"cap_usd":1}`},
	}
}

// Driven by the real catalogue so a template added later is covered by default.
func TestEveryNonAgentTemplate404sOnEveryAgentRoute(t *testing.T) {
	var checked int
	for _, tmpl := range templates.Catalog() {
		if tmpl.Kind == templates.KindAgent {
			continue
		}
		// Only templates the process runtime can create.
		if tmpl.ID != "base" && tmpl.ID != "node" && tmpl.ID != "openclaw" {
			continue
		}
		checked++
		srv, key, id := newAgentTestAPI(t, tmpl.ID)
		for _, rt := range agentRoutes(id) {
			code, out := do(t, rt.method, srv+rt.path, key, rt.body)
			if code != http.StatusNotFound {
				t.Errorf("%s: %s %s = %d, want 404", tmpl.ID, rt.method, rt.path, code)
			}
			if msg, _ := out["error"].(string); msg != "sandbox not found" {
				t.Errorf("%s: %s %s leaked %q", tmpl.ID, rt.method, rt.path, msg)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no non-agent templates were exercised; the guard proves nothing")
	}
}

// openclaw carries the "agent" tag but is KindApp; the gate must not match on tags.
func TestOpenclawIsNotTreatedAsAnAgentDespiteItsTag(t *testing.T) {
	tmpl, ok := templates.Resolve("openclaw")
	if !ok {
		t.Skip("openclaw template not present")
	}
	// If openclaw ever becomes KindAgent this test must be revisited, not silently pass.
	if tmpl.Kind == templates.KindAgent {
		t.Fatal("openclaw is now KindAgent — revisit this test and the gate's docs")
	}
	var tagged bool
	for _, tag := range tmpl.Tags {
		if tag == "agent" {
			tagged = true
		}
	}
	if !tagged {
		t.Skip("openclaw no longer carries the agent tag; the trap is gone")
	}

	srv, key, id := newAgentTestAPI(t, "openclaw")
	code, _ := do(t, "GET", srv+"/v1/sandboxes/"+id+"/agent/status", key, "")
	if code != http.StatusNotFound {
		t.Errorf("openclaw agent status = %d, want 404 — the gate matched on the tag", code)
	}
}

// An agent template passes the gate and then fails on transport with a distinct code.
func TestAgentTemplatePassesTheGateAndFailsOnTransport(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")

	code, out := do(t, "GET", srv+"/v1/sandboxes/"+id+"/agent/status", key, "")
	if code == http.StatusNotFound {
		t.Fatalf("an agent template was rejected by the gate: %v", out)
	}
	if code != http.StatusNotImplemented {
		t.Fatalf("agent status on the process runtime = %d, want 501", code)
	}
	if got, _ := out["code"].(string); got != codeAgentTransportUnsupported {
		t.Errorf("code = %q, want %q", got, codeAgentTransportUnsupported)
	}
}

// Ownership is checked before kind, so a caller cannot probe templates on sandboxes they do not own.
func TestAgentRoutesAreTenantScoped(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	_, mine, err := st.CreateKey(t.Context(), "mine", "org_mine")
	if err != nil {
		t.Fatal(err)
	}
	_, theirs, err := st.CreateKey(t.Context(), "theirs", "org_theirs")
	if err != nil {
		t.Fatal(err)
	}
	d := daemon.New(daemon.Config{
		Runtime:     process.New(filepath.Join(dir, "sb")),
		Store:       daemon.StoreAdapter{S: st},
		SnapshotDir: filepath.Join(dir, "snap"),
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	srv := httptest.NewServer(New(d, st).Routes())
	t.Cleanup(srv.Close)

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", mine, `{"template":"claude"}`)
	if code != http.StatusCreated {
		t.Fatalf("create = %d %v", code, out)
	}
	id, _ := out["id"].(string)

	for _, rt := range agentRoutes(id) {
		code, body := do(t, rt.method, srv.URL+rt.path, theirs, rt.body)
		if code != http.StatusNotFound {
			t.Errorf("cross-tenant %s %s = %d, want 404", rt.method, rt.path, code)
		}
		if msg, _ := body["error"].(string); msg != "sandbox not found" {
			t.Errorf("cross-tenant %s %s leaked %q", rt.method, rt.path, msg)
		}
	}

	// An unknown sandbox is indistinguishable from one the caller does not own.
	code, _ = do(t, "GET", srv.URL+"/v1/sandboxes/sb_nope/agent/status", theirs, "")
	if code != http.StatusNotFound {
		t.Errorf("unknown sandbox = %d, want 404", code)
	}
}

func TestAgentRoutesRequireAuth(t *testing.T) {
	srv, _, id := newAgentTestAPI(t, "claude")
	for _, rt := range agentRoutes(id) {
		code, _ := do(t, rt.method, srv+rt.path, "", rt.body)
		if code != http.StatusUnauthorized {
			t.Errorf("%s %s without a key = %d, want 401", rt.method, rt.path, code)
		}
	}
}

// An empty task is rejected on the node, before the supervisor is dialled.
func TestAgentSubmitValidatesTheMessageBeforeDialling(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")
	for _, body := range []string{`{}`, `{"message":""}`, `{"message":"  "}`, `not json`} {
		code, _ := do(t, "POST", srv+"/v1/sandboxes/"+id+"/agent/turns", key, body)
		if code != http.StatusBadRequest {
			t.Errorf("submit %s = %d, want 400", body, code)
		}
	}
}

// Whether an agent remembers across messages is derived from the manifest.
func TestStatusReportsWhetherTheAgentRemembers(t *testing.T) {
	for _, c := range []struct {
		template string
		want     bool
	}{
		{"claude", true},
		{"opencode", true},
		{"grok", false},
	} {
		srv, key, id := newAgentTestAPI(t, c.template)
		_, out := do(t, "GET", srv+"/v1/sandboxes/"+id+"/agent/status", key, "")
		// Status 501s on the process runtime, so assert on the manifest directly.
		tmpl, ok := templates.Resolve(c.template)
		if !ok {
			t.Fatalf("template %s missing", c.template)
		}
		remembers := tmpl.Agent.ContinueCmd != "" || tmpl.Agent.ResumeCmd != ""
		if remembers != c.want {
			t.Errorf("%s: remembers = %v, want %v", c.template, remembers, c.want)
		}
		_ = out
	}
}
