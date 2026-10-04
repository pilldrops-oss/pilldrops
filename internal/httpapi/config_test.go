package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestConfigAppliesToTheNextCommandWithoutRestart(t *testing.T) {
	srv, key, sb, _ := newCronSandbox(t)

	out := execIn(t, srv, key, sb, "printenv DATABASE_URL")
	if out != "" {
		t.Fatalf("DATABASE_URL already set to %q", out)
	}

	code, body := do(t, "POST", srv+"/v1/sandboxes/"+sb+"/config", key,
		`{"env":{"DATABASE_URL":"postgres://localhost/app","EXTRA":"two"}}`)
	if code != http.StatusOK {
		t.Fatalf("set config = %d %v", code, body)
	}

	if got := execIn(t, srv, key, sb, "printenv DATABASE_URL"); got != "postgres://localhost/app" {
		t.Errorf("DATABASE_URL = %q after config — a change that needs a restart is not a fix", got)
	}
	if got := execIn(t, srv, key, sb, "printenv EXTRA"); got != "two" {
		t.Errorf("second variable = %q, want two", got)
	}
}

func TestConfigMergesRatherThanReplaces(t *testing.T) {
	srv, key, sb, _ := newCronSandbox(t)

	do(t, "POST", srv+"/v1/sandboxes/"+sb+"/config", key, `{"env":{"FIRST":"1"}}`)
	do(t, "POST", srv+"/v1/sandboxes/"+sb+"/config", key, `{"env":{"SECOND":"2"}}`)

	if got := execIn(t, srv, key, sb, "printenv FIRST"); got != "1" {
		t.Errorf("FIRST = %q — the second write dropped it", got)
	}
	if got := execIn(t, srv, key, sb, "printenv SECOND"); got != "2" {
		t.Errorf("SECOND = %q", got)
	}
}

func TestConfigEmptyValueUnsets(t *testing.T) {
	srv, key, sb, _ := newCronSandbox(t)
	do(t, "POST", srv+"/v1/sandboxes/"+sb+"/config", key, `{"env":{"GONE":"here"}}`)
	if got := execIn(t, srv, key, sb, "printenv GONE"); got != "here" {
		t.Fatalf("setup failed, GONE = %q", got)
	}
	do(t, "POST", srv+"/v1/sandboxes/"+sb+"/config", key, `{"env":{"GONE":""}}`)
	if got := execIn(t, srv, key, sb, "printenv GONE"); got != "" {
		t.Errorf("GONE = %q after being cleared", got)
	}
}

func TestConfigReadBackMasksSecrets(t *testing.T) {
	srv, key, sb, _ := newCronSandbox(t)
	do(t, "POST", srv+"/v1/sandboxes/"+sb+"/config", key,
		`{"env":{"ANTHROPIC_API_KEY":"sk-ant-supersecret","PLAIN":"visible"}}`)

	code, out := do(t, "GET", srv+"/v1/sandboxes/"+sb+"/config", key, "")
	if code != http.StatusOK {
		t.Fatalf("get config = %d %v", code, out)
	}
	raw := strings.ToLower(jsonOf(out))
	if strings.Contains(raw, "supersecret") {
		t.Errorf("secret value returned to the client: %s", raw)
	}
	if !strings.Contains(raw, "anthropic_api_key") {
		t.Error("the key NAME must come back, or the UI cannot show what is set")
	}
	if !strings.Contains(raw, "plain") {
		t.Error("non-secret name missing")
	}
}

func TestConfigOwnership(t *testing.T) {
	srv, _, sb, st := newCronSandbox(t)
	_, otherKey, _ := st.CreateKey(t.Context(), "other", "org_other")
	for _, m := range []string{"GET", "POST"} {
		if code, _ := do(t, m, srv+"/v1/sandboxes/"+sb+"/config", otherKey, `{"env":{"X":"1"}}`); code != http.StatusNotFound {
			t.Errorf("cross-tenant %s config = %d, want 404", m, code)
		}
	}
}

func TestConfigRejectsBadNames(t *testing.T) {
	srv, key, sb, _ := newCronSandbox(t)
	for _, bad := range []string{`{"env":{"BAD NAME":"x"}}`, `{"env":{"BAD=NAME":"x"}}`, `{"env":{"":"x"}}`, `{"env":{"a;rm -rf /":"x"}}`} {
		if code, _ := do(t, "POST", srv+"/v1/sandboxes/"+sb+"/config", key, bad); code != http.StatusBadRequest {
			t.Errorf("%s did not 400", bad)
		}
	}
}

func execIn(t *testing.T, srv, key, sb, cmd string) string {
	t.Helper()
	_, out := do(t, "POST", srv+"/v1/sandboxes/"+sb+"/exec", key, `{"cmd":`+quoteJSON(cmd)+`}`)
	s, _ := out["stdout"].(string)
	return strings.TrimSpace(s)
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
