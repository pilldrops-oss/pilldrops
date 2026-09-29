package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	neturl "net/url"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/auth/orgassert"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
)

// asSandbox issues the request the control plane makes on behalf of a sandbox
// acting as itself: the operator credential and the sandbox named in the
// signed assertion's principal. Built by hand rather than mocked, because the
// bug was in exactly this relay.
func asSandbox(t *testing.T, method, url, opKey, org, actor string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, bytes.NewBufferString("{}"))
	req.Header.Set("Authorization", "Bearer "+opKey)
	req.Header.Set("Content-Type", "application/json")
	u, err := neturl.Parse(url)
	if err != nil {
		t.Fatalf("parse %q: %v", url, err)
	}
	keyID := sandbox.SelfKeyID(actor)
	tok := orgassert.Sign([]byte(testOrgOverrideSecret), org, testNodeID, method, u.Path, time.Now(), keyID, "sandbox")
	req.Header.Set("X-Org-Override", tok)
	req.Header.Set("X-Key-Id", keyID)
	req.Header.Set("X-Key-Role", "sandbox")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// A sandbox reaching its own private row is one question asked in two tiers,
// and both have to answer it the same way: the control plane names the caller,
// this node judges it. Covering only the naming, or only the rule, is how a
// private sandbox came to be unreachable by the identity token it booted with
// — and forks felt it first, being private by default.
func TestAPrivateSandboxIsReachableByItsOwnIdentity(t *testing.T) {
	srv, st, userKey := newTestServerWithStore(t)
	_, opKey, err := st.CreateKeyWithRole(t.Context(), "orch", "default", "operator")
	if err != nil {
		t.Fatalf("operator key: %v", err)
	}
	// Created by an ordinary key, private: the shape a fork child has.
	code, out := doOrg(t, "POST", srv.URL+"/v1/sandboxes", userKey, "",
		`{"template":"base","visibility":"private"}`)
	if code != http.StatusCreated {
		t.Fatalf("create = %d %v", code, out)
	}
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("no sandbox id in %v", out)
	}

	if code, out := asSandbox(t, "GET", srv.URL+"/v1/sandboxes/"+id, opKey, "default", id); code != 200 {
		t.Errorf("the sandbox cannot GET itself: %d %v", code, out)
	}
	if code, out := asSandbox(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/exec", opKey, "default", id); code == 404 {
		t.Errorf("the sandbox cannot exec in itself: %d %v", code, out)
	}
	code, out = asSandbox(t, "GET", srv.URL+"/v1/sandboxes", opKey, "default", id)
	if code != 200 || sandboxCount(out) != 1 {
		t.Errorf("the sandbox does not list itself: %d %v", code, out)
	}

	// The negative control: self is one sandbox, not every sandbox. Another
	// machine's identity must still be refused this private row.
	if code, _ := asSandbox(t, "GET", srv.URL+"/v1/sandboxes/"+id, opKey, "default", "sb-somebody-else"); code != 404 {
		t.Errorf("another sandbox's identity reached this private sandbox: %d", code)
	}
	if _, out := asSandbox(t, "GET", srv.URL+"/v1/sandboxes", opKey, "default", "sb-somebody-else"); sandboxCount(out) != 0 {
		t.Errorf("another sandbox's identity listed this private sandbox: %v", out)
	}
}
