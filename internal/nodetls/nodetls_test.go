package nodetls

import (
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every negative case carries its positive control in the same test, because
// "could not connect" and "refused this certificate" look alike.
func TestPinnedClientReachesTheNodeItPinnedAndRefusesAnother(t *testing.T) {
	nodeA := mustIdentity(t)
	nodeB := mustIdentity(t)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "node-a here")
	}))
	srv.TLS = nodeA.ServerConfig()
	srv.StartTLS()
	defer srv.Close()

	cfgA, err := PinnedClientConfig(nodeA.SPKI)
	if err != nil {
		t.Fatalf("PinnedClientConfig(A): %v", err)
	}
	body, err := get(srv.URL, cfgA)
	if err != nil {
		t.Fatalf("a client pinned to node A could not reach node A: %v", err)
	}
	if body != "node-a here" {
		t.Fatalf("body = %q", body)
	}

	cfgB, err := PinnedClientConfig(nodeB.SPKI)
	if err != nil {
		t.Fatalf("PinnedClientConfig(B): %v", err)
	}
	if _, err := get(srv.URL, cfgB); err == nil {
		t.Fatal("a client pinned to node B accepted node A's certificate — the pin is doing nothing")
	} else if !strings.Contains(err.Error(), ErrPinMismatch.Error()) {
		t.Fatalf("connection failed for the wrong reason: %v\n"+
			"want a pin mismatch; anything else means the test proved reachability, not verification", err)
	}

	// The system trust store must not accept the self-signed certificate either.
	if _, err := get(srv.URL, nil); err == nil {
		t.Fatal("the system trust store accepted a self-signed node certificate")
	}
}

func TestIdentityIsStableAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first.SPKI != second.SPKI {
		t.Errorf("pin changed across a reload: %s then %s", first.SPKI, second.SPKI)
	}
	// Guards against the check above passing for a hard-coded constant.
	other, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("other: %v", err)
	}
	if other.SPKI == first.SPKI {
		t.Error("two independently generated identities share a pin")
	}
}

func TestPrivateKeyIsNotWorldReadable(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadOrCreate(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "node-tls.key"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("node-tls.key mode = %o, want no group/other bits", perm)
	}
}

func TestUnusableKeypairIsRefusedRatherThanRegenerated(t *testing.T) {
	dir := t.TempDir()
	orig, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node-tls.key"), []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadOrCreate(dir)
	if err == nil {
		t.Fatalf("a corrupt keypair silently produced an identity (pin %s, was %s)", got.SPKI, orig.SPKI)
	}
}

func TestEmptyPinIsRefused(t *testing.T) {
	if cfg, err := PinnedClientConfig(""); err == nil {
		t.Fatalf("PinnedClientConfig(\"\") returned a config (InsecureSkipVerify=%v) instead of an error",
			cfg.InsecureSkipVerify)
	}
	if err := VerifyPin("", nil); err == nil {
		t.Fatal("VerifyPin with no pin returned nil — it would accept any certificate")
	}
	if err := VerifyPin("some-pin", nil); !errors.Is(err, ErrPinMismatch) {
		t.Errorf("VerifyPin with no certificate = %v, want ErrPinMismatch", err)
	}
}

func TestRequireSecureHopReadsTheEnvironment(t *testing.T) {
	t.Setenv(RequireSecureHopEnv, "")
	if RequireSecureHop() {
		t.Error("unset must default to off — see the doc comment for why this one is not fail-closed")
	}
	t.Setenv(RequireSecureHopEnv, "1")
	if !RequireSecureHop() {
		t.Error("=1 must enable enforcement")
	}
	t.Setenv(RequireSecureHopEnv, "0")
	if RequireSecureHop() {
		t.Error("=0 must disable enforcement")
	}
}

func mustIdentity(t *testing.T) *Identity {
	t.Helper()
	id, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	return id
}

// get fetches url with the given TLS config; nil means system defaults.
func get(url string, cfg *tls.Config) (string, error) {
	tr := &http.Transport{TLSClientConfig: cfg}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr}).Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}
