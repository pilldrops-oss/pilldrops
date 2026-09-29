package orchestrator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

func newVaultServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "v.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.SetSecretKey("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatalf("key: %v", err)
	}
	s := NewServer("fleet-key", "nodetoken")
	s.SetStore(st)
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	return srv, st
}

func TestVaultRoutesAreServedByTheControlPlane(t *testing.T) {
	srv, _ := newVaultServer(t)

	code, _ := do(t, srv, "POST", "/v1/secrets", "fleet-key",
		`{"name":"ANTHROPIC_API_KEY","value":"sk-ant-wxyz"}`)
	if code != http.StatusOK {
		t.Fatalf("put = %d — /v1/secrets must not 404 on this tier", code)
	}

	req, _ := http.NewRequest("GET", srv.URL+"/v1/secrets", nil)
	req.Header.Set("Authorization", "Bearer fleet-key")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list []store.VaultSecret
	_ = json.NewDecoder(resp.Body).Decode(&list)
	if len(list) != 1 || list[0].Name != "ANTHROPIC_API_KEY" {
		t.Fatalf("list = %+v", list)
	}
	if list[0].Last4 != "wxyz" {
		t.Errorf("last4 = %q, want wxyz", list[0].Last4)
	}
	if list[0].Value != "" {
		t.Errorf("the API returned the credential: %q", list[0].Value)
	}

	if code, _ := do(t, srv, "DELETE", "/v1/secrets/ANTHROPIC_API_KEY", "fleet-key", ""); code != http.StatusNoContent {
		t.Errorf("delete = %d, want 204", code)
	}
	if code, _ := do(t, srv, "DELETE", "/v1/secrets/ANTHROPIC_API_KEY", "fleet-key", ""); code != http.StatusNotFound {
		t.Errorf("second delete = %d, want 404", code)
	}
}

func TestVaultIsScopedToTheAuthenticatedOrg(t *testing.T) {
	srv, st := newVaultServer(t)
	_, aKey, _ := st.CreateKey(t.Context(), "a", "u:alice")
	_, bKey, _ := st.CreateKey(t.Context(), "b", "u:bob")

	if code, _ := do(t, srv, "POST", "/v1/secrets", aKey,
		`{"name":"ANTHROPIC_API_KEY","value":"alice-key-1234"}`); code != http.StatusOK {
		t.Fatalf("alice put = %d", code)
	}

	req, _ := http.NewRequest("GET", srv.URL+"/v1/secrets", nil)
	req.Header.Set("Authorization", "Bearer "+bKey)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("bob list: %v", err)
	}
	defer resp.Body.Close()
	var list []store.VaultSecret
	_ = json.NewDecoder(resp.Body).Decode(&list)
	if len(list) != 0 {
		t.Errorf("bob sees %d of alice's secrets", len(list))
	}
	if code, _ := do(t, srv, "DELETE", "/v1/secrets/ANTHROPIC_API_KEY", bKey, ""); code != http.StatusNotFound {
		t.Errorf("bob deleted alice's secret: %d", code)
	}
}

func TestSecretRefsResolveOnCreateAndAreStripped(t *testing.T) {
	srv, st := newVaultServer(t)
	s := NewServer("fleet-key", "nodetoken")
	s.SetStore(st)

	if _, err := st.PutSecret(t.Context(), "default", "ANTHROPIC_API_KEY", "sk-resolved-9999", true, false); err != nil {
		t.Fatal(err)
	}
	_ = srv

	req := httptest.NewRequest("POST", "/v1/sandboxes", nil)
	req = req.WithContext(withOperator(req.Context()))
	out, err := s.resolveSecretRefs(req,
		[]byte(`{"template":"base","secret_refs":["ANTHROPIC_API_KEY"]}`))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got := string(out)
	if strings.Contains(got, "secret_refs") {
		t.Errorf("secret_refs survived into the forwarded body: %s", got)
	}
	if !strings.Contains(got, "ANTHROPIC_API_KEY=sk-resolved-9999") {
		t.Errorf("the secret was not resolved into env: %s", got)
	}
}

// An auto_inject entry reaches a create that names no secret_refs; another
// org's create does not see it; an explicit env entry wins the name.
func TestAutoInjectSecretsReachPlainCreates(t *testing.T) {
	_, st := newVaultServer(t)
	s := NewServer("fleet-key", "nodetoken")
	s.SetStore(st)
	if _, err := st.PutSecret(t.Context(), "default", "TOK", "auto-value", true, true); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutSecret(t.Context(), "default", "URL", "https://auto", false, true); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/v1/sandboxes", nil)
	req = req.WithContext(withOperator(req.Context()))

	out, err := s.resolveSecretRefs(req, []byte(`{"template":"base","env":["URL=https://mine"]}`))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, "TOK=auto-value") {
		t.Errorf("auto_inject entry missing from a plain create: %s", got)
	}
	if !strings.Contains(got, "URL=https://mine") || strings.Contains(got, "URL=https://auto") {
		t.Errorf("explicit env must win over auto_inject: %s", got)
	}

	// Tenant control: a different org's create carries none of it.
	breq := httptest.NewRequest("POST", "/v1/sandboxes", nil)
	breq = breq.WithContext(withOrg(breq.Context(), "u:bob"))
	bout, err := s.resolveSecretRefs(breq, []byte(`{"template":"base"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(bout), "auto-value") {
		t.Errorf("another org received the first org's auto_inject entry: %s", bout)
	}
}

// A sandbox launched without its key fails later inside the guest, where the cause is invisible.
func TestUnresolvableSecretRefFailsTheCreate(t *testing.T) {
	_, st := newVaultServer(t)
	s := NewServer("fleet-key", "nodetoken")
	s.SetStore(st)

	req := httptest.NewRequest("POST", "/v1/sandboxes", nil)
	req = req.WithContext(withOperator(req.Context()))
	_, err := s.resolveSecretRefs(req, []byte(`{"secret_refs":["NEVER_CONNECTED"]}`))
	if err == nil {
		t.Fatal("a create naming an unknown secret succeeded")
	}
	if !strings.Contains(err.Error(), "NEVER_CONNECTED") {
		t.Errorf("error does not name the secret: %v", err)
	}
}

func TestBodiesWithoutSecretRefsPassThrough(t *testing.T) {
	_, st := newVaultServer(t)
	s := NewServer("fleet-key", "nodetoken")
	s.SetStore(st)
	req := httptest.NewRequest("POST", "/v1/sandboxes", nil)

	for _, in := range []string{`{"template":"base"}`, `not json at all`, ``} {
		out, err := s.resolveSecretRefs(req, []byte(in))
		if err != nil {
			t.Errorf("%q returned an error: %v", in, err)
		}
		if string(out) != in {
			t.Errorf("%q was rewritten to %q", in, string(out))
		}
	}
}
