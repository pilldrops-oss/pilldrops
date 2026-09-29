//go:build !core

package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

func TestCreateEnvReachesTheSandbox(t *testing.T) {
	srv, _, key := newTestServerWithStore(t)

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key,
		`{"template":"base","env":["ANTHROPIC_API_KEY=sk-ant-e2e","PLAIN=value-two"]}`)
	if code != http.StatusCreated {
		t.Fatalf("create = %d %v", code, out)
	}
	id, _ := out["id"].(string)

	code, out = do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/exec", key,
		`{"cmd":"printenv ANTHROPIC_API_KEY"}`)
	if code != http.StatusOK {
		t.Fatalf("exec = %d %v", code, out)
	}
	if got := strings.TrimSpace(outStr(out, "stdout")); got != "sk-ant-e2e" {
		t.Errorf("ANTHROPIC_API_KEY inside the sandbox = %q, want %q — BYOK is the product and it is not arriving", got, "sk-ant-e2e")
	}

	// A bug that passes only the first variable would pass every single-key test.
	code, out = do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/exec", key, `{"cmd":"printenv PLAIN"}`)
	if code != http.StatusOK {
		t.Fatalf("exec = %d %v", code, out)
	}
	if got := strings.TrimSpace(outStr(out, "stdout")); got != "value-two" {
		t.Errorf("second env var = %q, want %q — only the first is being applied", got, "value-two")
	}
}

// Base64 secrets and connection strings contain '='; split on the first only.
func TestCreateEnvKeepsEqualsInValues(t *testing.T) {
	srv, _, key := newTestServerWithStore(t)

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key,
		`{"template":"base","env":["TOKEN=abc==def=","EMPTY="]}`)
	if code != http.StatusCreated {
		t.Fatalf("create = %d %v", code, out)
	}
	id, _ := out["id"].(string)

	code, out = do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/exec", key, `{"cmd":"printenv TOKEN"}`)
	if code != http.StatusOK {
		t.Fatalf("exec = %d %v", code, out)
	}
	if got := strings.TrimSpace(outStr(out, "stdout")); got != "abc==def=" {
		t.Errorf("TOKEN = %q, want %q — the value was split on the wrong '='", got, "abc==def=")
	}
}

func outStr(out map[string]any, k string) string {
	s, _ := out[k].(string)
	return s
}
