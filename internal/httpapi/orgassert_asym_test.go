package httpapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/auth/orgassert"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/process"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// A node holding only the public key must verify a genuine assertion and be
// unable to mint one, so a compromised node cannot act as another tenant.
func TestNodeWithThePublicKeyVerifiesButCannotForge(t *testing.T) {
	priv, err := orgassert.ParsePrivateKey("00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	if err != nil {
		t.Fatalf("ParsePrivateKey: %v", err)
	}
	pub, err := orgassert.ParsePublicKey(orgassert.PublicKeyHex(priv))
	if err != nil {
		t.Fatalf("ParsePublicKey: %v", err)
	}

	// Built by hand because newTestServerWithStore does not return the *Server.
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	_, opKey, err := st.CreateKeyWithRole(t.Context(), "orch", "default", "operator")
	if err != nil {
		t.Fatalf("operator key: %v", err)
	}
	d := daemon.New(daemon.Config{
		Runtime:     process.New(filepath.Join(dir, "sb")),
		Store:       daemon.StoreAdapter{S: st},
		SnapshotDir: filepath.Join(dir, "snap"),
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	api := New(d, st)
	api.SetOrgOverrideSecret([]byte(testOrgOverrideSecret))
	api.SetOrgAssertPublicKey(pub)
	api.SetNodeID(testNodeID)
	srv := httptest.NewServer(api.Routes())
	t.Cleanup(srv.Close)

	call := func(hdr string) int {
		req, _ := http.NewRequest("GET", srv.URL+"/v1/sandboxes", nil)
		req.Header.Set("Authorization", "Bearer "+opKey)
		req.Header.Set("X-Org-Override", hdr)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	genuine := orgassert.SignAsym(priv, "u:cust1", testNodeID, "GET", "/v1/sandboxes", time.Now())
	if code := call(genuine); code != http.StatusOK {
		t.Errorf("a genuine Ed25519 assertion was refused: %d", code)
	}

	// Everything a compromised node could sign with: the public key and the shared secrets.
	forgeries := map[string]string{
		"HMAC keyed on the public key":  orgassert.Sign(pub, "u:victim", testNodeID, "GET", "/v1/sandboxes", time.Now()),
		"HMAC keyed on the fleet token": orgassert.Sign([]byte(testOrgOverrideSecret), "u:victim", testNodeID, "GET", "/v1/sandboxes", time.Now()),
		"a bare org string":             "u:victim",
	}
	for name, hdr := range forgeries {
		if hdr == "" {
			continue
		}
		if code := call(hdr); code == http.StatusOK {
			t.Errorf("%s was accepted — a node can still act as another tenant", name)
		}
	}
}
