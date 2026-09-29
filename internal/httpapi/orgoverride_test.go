package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/auth/orgassert"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/process"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// An operator key may act for a customer org via a signed X-Org-Override; a
// user key must not.

// doOrg is do() plus a signed X-Org-Override header.
func doOrg(t *testing.T, method, url, key, org, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+key)
	if org != "" {
		// The assertion is bound to method+path.
		u, perr := neturl.Parse(url)
		if perr != nil {
			t.Fatalf("parse %q: %v", url, perr)
		}
		tok := orgassert.Sign([]byte(testOrgOverrideSecret), org, testNodeID, method, u.Path, time.Now())
		req.Header.Set("X-Org-Override", tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func sandboxCount(out map[string]any) int {
	arr, _ := out["sandboxes"].([]any)
	return len(arr)
}

func TestOrgOverrideOperatorActsAsCustomerOrg(t *testing.T) {
	srv, st, _ := newTestServerWithStore(t)
	_, opKey, err := st.CreateKeyWithRole(t.Context(), "orch", "default", "operator")
	if err != nil {
		t.Fatalf("operator key: %v", err)
	}

	code, out := doOrg(t, "POST", srv.URL+"/v1/sandboxes", opKey, "u:cust1", `{"template":"base"}`)
	if code != http.StatusCreated {
		t.Fatalf("create with override = %d (%v)", code, out)
	}

	for _, tc := range []struct {
		override string
		want     int
	}{
		{"u:cust1", 1},
		{"", 0}, // operator's own org
		{"u:other", 0},
	} {
		code, out := doOrg(t, "GET", srv.URL+"/v1/sandboxes", opKey, tc.override, "")
		if code != http.StatusOK {
			t.Fatalf("list override=%q = %d", tc.override, code)
		}
		if got := sandboxCount(out); got != tc.want {
			t.Errorf("list override=%q: %d sandboxes, want %d", tc.override, got, tc.want)
		}
	}
}

func TestOrgOverrideUserKeyCannotImpersonate(t *testing.T) {
	srv, st, _ := newTestServerWithStore(t)
	_, opKey, err := st.CreateKeyWithRole(t.Context(), "orch", "default", "operator")
	if err != nil {
		t.Fatalf("operator key: %v", err)
	}
	_, userKey, err := st.CreateKeyWithRole(t.Context(), "cust2-key", "u:cust2", "user")
	if err != nil {
		t.Fatalf("user key: %v", err)
	}

	if code, out := doOrg(t, "POST", srv.URL+"/v1/sandboxes", opKey, "u:cust1", `{"template":"base"}`); code != http.StatusCreated {
		t.Fatalf("seed create = %d (%v)", code, out)
	}

	code, out := doOrg(t, "GET", srv.URL+"/v1/sandboxes", userKey, "u:cust1", "")
	if code != http.StatusOK {
		t.Fatalf("user list = %d", code)
	}
	if got := sandboxCount(out); got != 0 {
		t.Errorf("user key with override header saw %d foreign sandboxes, want 0", got)
	}
}

func TestOrgOverrideRejectedWithoutValidSignature(t *testing.T) {
	srv, st, _ := newTestServerWithStore(t)
	_, opKey, err := st.CreateKeyWithRole(t.Context(), "orch", "default", "operator")
	if err != nil {
		t.Fatalf("operator key: %v", err)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/v1/sandboxes", nil)
	req.Header.Set("Authorization", "Bearer "+opKey)
	req.Header.Set("X-Org-Override", "u:cust1") // bare, unsigned
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bare unsigned override = %d, want 403 (refused, not silently honored or downgraded)", resp.StatusCode)
	}
}

func TestOrgOverrideRejectedWhenTampered(t *testing.T) {
	srv, st, _ := newTestServerWithStore(t)
	_, opKey, err := st.CreateKeyWithRole(t.Context(), "orch", "default", "operator")
	if err != nil {
		t.Fatalf("operator key: %v", err)
	}
	tok := orgassert.Sign([]byte(testOrgOverrideSecret), "u:cust1", testNodeID, "GET", "/v1/sandboxes", time.Now())
	tampered := strings.Replace(tok, "u:cust1", "u:victim", 1)

	req, _ := http.NewRequest("GET", srv.URL+"/v1/sandboxes", nil)
	req.Header.Set("Authorization", "Bearer "+opKey)
	req.Header.Set("X-Org-Override", tampered)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("tampered override = %d, want 403", resp.StatusCode)
	}
}

func TestOrgOverrideRejectedWhenExpired(t *testing.T) {
	srv, st, _ := newTestServerWithStore(t)
	_, opKey, err := st.CreateKeyWithRole(t.Context(), "orch", "default", "operator")
	if err != nil {
		t.Fatalf("operator key: %v", err)
	}
	stale := orgassert.Sign([]byte(testOrgOverrideSecret), "u:cust1", testNodeID, "GET", "/v1/sandboxes", time.Now().Add(-orgassert.TTL-orgassert.MaxSkew-time.Second))

	req, _ := http.NewRequest("GET", srv.URL+"/v1/sandboxes", nil)
	req.Header.Set("Authorization", "Bearer "+opKey)
	req.Header.Set("X-Org-Override", stale)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expired override = %d, want 403", resp.StatusCode)
	}
}

func TestOrgOverrideRejectedWhenSignedByWrongSecret(t *testing.T) {
	srv, st, _ := newTestServerWithStore(t)
	_, opKey, err := st.CreateKeyWithRole(t.Context(), "orch", "default", "operator")
	if err != nil {
		t.Fatalf("operator key: %v", err)
	}
	forged := orgassert.Sign([]byte("not-the-real-secret"), "u:cust1", testNodeID, "GET", "/v1/sandboxes", time.Now())

	req, _ := http.NewRequest("GET", srv.URL+"/v1/sandboxes", nil)
	req.Header.Set("Authorization", "Bearer "+opKey)
	req.Header.Set("X-Org-Override", forged)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("forged/wrong-secret override = %d, want 403", resp.StatusCode)
	}
}

// With no secret configured, DEV_INSECURE trusts a bare org string.
func TestOrgOverrideDevInsecureFallsBackToBareWhenUnconfigured(t *testing.T) {
	t.Setenv("DEV_INSECURE", "1")
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
	s := New(d, st) // no SetOrgOverrideSecret
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	_, opKey, err := st.CreateKeyWithRole(t.Context(), "orch", "default", "operator")
	if err != nil {
		t.Fatalf("operator key: %v", err)
	}

	req, _ := http.NewRequest("POST", srv.URL+"/v1/sandboxes", bytes.NewBufferString(`{"template":"base"}`))
	req.Header.Set("Authorization", "Bearer "+opKey)
	req.Header.Set("X-Org-Override", "u:cust1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("DEV_INSECURE with no secret configured: bare override = %d, want 201 (dev escape hatch honored)", resp.StatusCode)
	}
}

func TestOrgOverrideUnconfiguredWithoutDevInsecureRefuses(t *testing.T) {
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
	s := New(d, st) // no SetOrgOverrideSecret, no DEV_INSECURE
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	_, opKey, err := st.CreateKeyWithRole(t.Context(), "orch", "default", "operator")
	if err != nil {
		t.Fatalf("operator key: %v", err)
	}

	req, _ := http.NewRequest("POST", srv.URL+"/v1/sandboxes", bytes.NewBufferString(`{"template":"base"}`))
	req.Header.Set("Authorization", "Bearer "+opKey)
	req.Header.Set("X-Org-Override", "u:cust1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unconfigured secret, no DEV_INSECURE: bare override = %d, want 403", resp.StatusCode)
	}
}

func TestOrgOverrideBillingIsPerCustomerOrg(t *testing.T) {
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
	s := New(d, st)
	s.SetOrgOverrideSecret([]byte(testOrgOverrideSecret))
	s.SetNodeID(testNodeID)
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)

	_, opKey, err := st.CreateKeyWithRole(t.Context(), "orch", "default", "operator")
	if err != nil {
		t.Fatalf("operator key: %v", err)
	}
	if err := st.SetPlan(t.Context(), "u:cust3", "pro"); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}

	code, out := doOrg(t, "GET", srv.URL+"/v1/limits", opKey, "u:cust3", "")
	if code != http.StatusOK {
		t.Fatalf("limits = %d (%v)", code, out)
	}
	plan, _ := out["plan"].(map[string]any)
	if id, _ := plan["id"].(string); id != "pro" {
		t.Errorf("plan = %q, want %q (customer org's plan, not operator's)", id, "pro")
	}
}
