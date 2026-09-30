package httpapi

import (
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/sandbox"

	"github.com/pilldrops-oss/pilldrops/internal/pdconfig"
)

func cfg() pdconfig.Config {
	c := pdconfig.Config{
		Version: 1, Template: "playwright",
		Setup: []string{"npm ci"}, Start: "npx playwright test", Port: 9323,
	}
	c.Resources.VCPU = 2
	c.Resources.MemoryMB = 2048
	return c
}

func TestMergeAdoptsEverythingWhenRequestIsSilent(t *testing.T) {
	got := mergePilldropsConfig(createReq{Spec: sandbox.Spec{}}, cfg())
	if got.Template != "playwright" || got.Start != "npx playwright test" {
		t.Fatalf("template/start = %q %q", got.Template, got.Start)
	}
	if got.VCPU != 2 || got.MemoryMB != 2048 || got.Port != 9323 {
		t.Fatalf("resources/port = %d %d %d", got.VCPU, got.MemoryMB, got.Port)
	}
	if len(got.Setup) != 1 {
		t.Fatalf("setup = %v", got.Setup)
	}
}

func TestMergeExplicitTemplateWins(t *testing.T) {
	got := mergePilldropsConfig(createReq{Spec: sandbox.Spec{Template: "node"}}, cfg())
	if got.Template != "node" {
		t.Fatalf("Template = %q, want the request's", got.Template)
	}
	if got.Start != "npx playwright test" {
		t.Errorf("Start should come from the file, got %q", got.Start)
	}
}

func TestMergeExplicitResourcesWinFieldByField(t *testing.T) {
	got := mergePilldropsConfig(createReq{Spec: sandbox.Spec{VCPU: 8}}, cfg())
	if got.VCPU != 8 {
		t.Errorf("VCPU = %d, want the request's 8", got.VCPU)
	}
	if got.MemoryMB != 2048 {
		t.Errorf("MemoryMB = %d, want the file's 2048", got.MemoryMB)
	}
}

func TestMergeExplicitSetupWins(t *testing.T) {
	got := mergePilldropsConfig(createReq{Spec: sandbox.Spec{Setup: []string{"make deps"}}}, cfg())
	if len(got.Setup) != 1 || got.Setup[0] != "make deps" {
		t.Fatalf("Setup = %v, want the request's", got.Setup)
	}
}

func TestMergeLeavesUnrelatedFieldsAlone(t *testing.T) {
	got := mergePilldropsConfig(createReq{Spec: sandbox.Spec{Name: "nightly", Timeout: 600}}, cfg())
	if got.Name != "nightly" || got.Timeout != 600 {
		t.Fatalf("unrelated fields changed: %+v", got)
	}
}

func TestCreateAdoptsPilldropsJSON(t *testing.T) {
	srv, key := newTestServer(t)
	body := `{"files":[{"path":"pilldrops.json","content":"{\"version\":1,\"template\":\"node\",\"start\":\"echo hi\"}"}]}`
	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, body)
	if code != 201 {
		t.Fatalf("create = %d (%v)", code, out)
	}
	if tmpl, _ := out["template"].(string); tmpl != "node" {
		t.Fatalf("template = %v, want node (from the file)", out["template"])
	}
}

// A 400 that had already provisioned a sandbox would bill for a launch the caller was told failed.
func TestCreateMalformedPilldropsJSONIs400AndCreatesNothing(t *testing.T) {
	srv, key := newTestServer(t)
	body := `{"files":[{"path":"pilldrops.json","content":"{not json"}]}`
	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, body)
	if code != 400 {
		t.Fatalf("create = %d (%v), want 400", code, out)
	}
	if code, out := do(t, "GET", srv.URL+"/v1/sandboxes", key, ""); code != 200 {
		t.Fatalf("list = %d %v", code, out)
	} else if sbs, _ := out["sandboxes"].([]any); len(sbs) != 0 {
		t.Fatalf("expected no sandboxes created, got %v", sbs)
	}
}

func TestCreateUnknownTemplateInFileIs400NamingTheID(t *testing.T) {
	srv, key := newTestServer(t)
	body := `{"files":[{"path":"pilldrops.json","content":"{\"version\":1,\"template\":\"kubernetes\"}"}]}`
	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, body)
	if code != 400 {
		t.Fatalf("create = %d (%v), want 400", code, out)
	}
	if !contains(fmtErr(out), "kubernetes") {
		t.Errorf("error should name the id: %v", out)
	}
}

func TestCreateEnvValuesInFileIs400WithoutEchoingTheSecret(t *testing.T) {
	srv, key := newTestServer(t)
	body := `{"files":[{"path":"pilldrops.json","content":"{\"version\":1,\"template\":\"base\",\"env\":{\"K\":\"sk_live_leak\"}}"}]}`
	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, body)
	if code != 400 {
		t.Fatalf("create = %d (%v), want 400", code, out)
	}
	if contains(fmtErr(out), "sk_live_leak") {
		t.Fatalf("response echoed the secret back: %v", out)
	}
}

func fmtErr(out map[string]any) string {
	if e, ok := out["error"].(string); ok {
		return e
	}
	return ""
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
