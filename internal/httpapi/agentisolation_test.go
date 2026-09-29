//go:build !core

package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsolationDoesNotClaimAMicroVMOnAContainerNode(t *testing.T) {
	for _, runtime := range []string{"process", "docker", ""} {
		fact := (&Server{}).runtimeFactFor(runtime)
		if fact.State == "yes" {
			t.Errorf("runtime %q reported as a microVM", runtime)
		}
		if fact.Detail == "" {
			t.Errorf("runtime %q gave no explanation, leaving a bare ✗", runtime)
		}
	}
	if got := (&Server{}).runtimeFactFor("firecracker"); got.State != "yes" {
		t.Errorf("firecracker reported as %q — the card understates a real VM boundary", got.State)
	}
	if got := (&Server{}).runtimeFactFor("gvisor-nextgen"); got.State != "unknown" {
		t.Errorf("an unknown runtime reported %q, want unknown", got.State)
	}
}

func TestIsolationTLSReflectsTheActualConnection(t *testing.T) {
	for _, tc := range []struct{ proto, want string }{
		{"https", "yes"},
		{"http", "no"},
		{"", "unknown"},
	} {
		r := httptest.NewRequest("GET", "/x", nil)
		if tc.proto != "" {
			r.Header.Set("X-Forwarded-Proto", tc.proto)
		}
		if got := isolationTLSFact(r); got.State != tc.want {
			t.Errorf("proto %q gave %q, want %q", tc.proto, got.State, tc.want)
		}
	}
}

func TestIsolationTellsTheTruthAboutStoredKeys(t *testing.T) {
	srv, key, sb, st := newCronSandbox(t)
	_ = srv
	_ = key
	api := &Server{st: st}

	if got := api.isolationKeyFact(t.Context(), sb); got.State != "yes" {
		t.Errorf("with no stored credential the card says %q, want yes", got.State)
	}

	if _, err := st.MergeSandboxConfig(t.Context(), sb, "default",
		map[string]string{"ANTHROPIC_API_KEY": "sk-ant-x", "TZ": "UTC"}, "", ""); err != nil {
		t.Fatalf("merge: %v", err)
	}
	got := api.isolationKeyFact(t.Context(), sb)
	if got.State != "no" {
		t.Fatalf("a stored credential is reported as %q — the card is claiming something false", got.State)
	}
	// Names the stored credential without leaking its value.
	if !strings.Contains(got.Detail, "ANTHROPIC_API_KEY") {
		t.Errorf("does not name the stored credential: %q", got.Detail)
	}
	if strings.Contains(got.Detail, "sk-ant-x") {
		t.Fatalf("the card leaks the key VALUE: %q", got.Detail)
	}
	if strings.Contains(got.Detail, "TZ") {
		t.Errorf("listed a non-credential variable as a credential: %q", got.Detail)
	}
}

// The card names stored credentials, which another tenant must not enumerate.
func TestIsolationRouteRequiresOwnership(t *testing.T) {
	srv, _, sb, _ := newCronSandbox(t)
	code, _ := do(t, "GET", srv+"/v1/sandboxes/"+sb+"/agent/isolation", "sf_not_a_real_key", "")
	if code != http.StatusUnauthorized && code != http.StatusNotFound {
		t.Errorf("unauthenticated read = %d, want 401 or 404", code)
	}
}

// Silently dropping an unknown mode would launch wider than the caller asked for.
func TestLaunchRejectsAnUnknownAgentMode(t *testing.T) {
	srv, key, _, _ := newCronSandbox(t)

	code, out := do(t, "POST", srv+"/v1/sandboxes", key,
		`{"template":"base","agent_mode":"yolo"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("create = %d %v, want 400", code, out)
	}
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "guided") {
		t.Errorf("the refusal does not name the valid options: %q", msg)
	}
}

// A caller must not shadow the names the platform sets; the refusal names the
// prefix. The platform's own PILLDROPS_AGENT_MODE is appended after the check.
func TestCreateRefusesReservedEnvPrefixes(t *testing.T) {
	srv, key, _, _ := newCronSandbox(t)
	for _, name := range []string{"PD_SANDBOX_ID", "PILLDROPS_X", "1BAD"} {
		code, out := do(t, "POST", srv+"/v1/sandboxes", key,
			`{"template":"base","env":["`+name+`=x"]}`)
		if code != http.StatusBadRequest {
			t.Fatalf("create with %s = %d %v, want 400", name, code, out)
		}
		msg, _ := out["error"].(string)
		if !strings.Contains(msg, name) {
			t.Errorf("refusal does not name %s: %q", name, msg)
		}
	}
	code, out := do(t, "POST", srv+"/v1/sandboxes", key,
		`{"template":"base","env":["PD_SANDBOX_ID=x"]}`)
	if msg, _ := out["error"].(string); code != 400 || !strings.Contains(msg, "PD_") {
		t.Errorf("refusal must name the reserved prefix: %d %q", code, msg)
	}
	// Positive control: lower-case and the platform's own append still work.
	code, out = do(t, "POST", srv+"/v1/sandboxes", key,
		`{"template":"base","env":["lower_case=1"],"agent_mode":"guided"}`)
	if code != http.StatusCreated {
		t.Fatalf("lower_case env = %d %v", code, out)
	}
	id, _ := out["id"].(string)
	code, out = do(t, "POST", srv+"/v1/sandboxes/"+id+"/exec", key,
		`{"cmd":"echo mode=$PILLDROPS_AGENT_MODE lc=$lower_case"}`)
	if stdout, _ := out["stdout"].(string); code != 200 || !strings.Contains(stdout, "mode=guided lc=1") {
		t.Errorf("platform env did not reach the sandbox: %d %v", code, out)
	}
}

func TestLaunchAcceptsEachLadderRung(t *testing.T) {
	srv, key, _, _ := newCronSandbox(t)
	for _, mode := range []string{"supervised", "guided", "autonomous"} {
		code, out := do(t, "POST", srv+"/v1/sandboxes", key,
			`{"template":"base","agent_mode":"`+mode+`"}`)
		if code != http.StatusCreated {
			t.Errorf("agent_mode %q = %d %v", mode, code, out)
		}
	}
}
