package store

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// openConfigStore opens a store with at-rest encryption enabled.
func openConfigStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.SetSecretKey(hex.EncodeToString(bytes.Repeat([]byte{9}, 32))); err != nil {
		t.Fatalf("SetSecretKey: %v", err)
	}
	return s, path
}

// Asserted on the raw column; a round-trip passes whether or not anything was encrypted.
func TestSandboxConfigEnvIsEncryptedAtRest(t *testing.T) {
	s, _ := openConfigStore(t)

	if _, err := s.MergeSandboxConfig(t.Context(), "sb1", "org1",
		map[string]string{"ANTHROPIC_API_KEY": "sk-ant-notarealkey-0123456789abcdef"},
		"", ""); err != nil {
		t.Fatalf("merge: %v", err)
	}

	var raw string
	if err := s.queryRow(t.Context(), `SELECT env FROM sandbox_config WHERE sandbox_id = ?`, "sb1").Scan(&raw); err != nil {
		t.Fatalf("read raw column: %v", err)
	}
	if strings.Contains(raw, "sk-ant-notarealkey") {
		t.Fatalf("the provider key is on disk in the clear: %q", raw)
	}
	if !strings.HasPrefix(raw, "enc:v1:") {
		t.Errorf("stored value is not sealed: %q", raw)
	}

	got := s.SandboxConfigFor(t.Context(), "sb1")
	if got.Env["ANTHROPIC_API_KEY"] != "sk-ant-notarealkey-0123456789abcdef" {
		t.Errorf("round-trip lost the value: %v", got.Env)
	}
}

func TestSandboxConfigReadsLegacyPlaintext(t *testing.T) {
	s, _ := openConfigStore(t)

	if _, err := s.exec(t.Context(),
		`INSERT INTO sandbox_config (sandbox_id, org_id, env, model, base_url, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"sb2", "org1", `{"OPENAI_API_KEY":"legacy-value"}`, "", "", 0); err != nil {
		t.Fatalf("insert legacy: %v", err)
	}
	got := s.SandboxConfigFor(t.Context(), "sb2")
	if got.Env["OPENAI_API_KEY"] != "legacy-value" {
		t.Fatalf("a pre-encryption row no longer reads: %v", got.Env)
	}

	// A merge re-seals the legacy row.
	if _, err := s.MergeSandboxConfig(t.Context(), "sb2", "org1", map[string]string{"X": "y"}, "", ""); err != nil {
		t.Fatalf("merge: %v", err)
	}
	var raw string
	_ = s.queryRow(t.Context(), `SELECT env FROM sandbox_config WHERE sandbox_id = ?`, "sb2").Scan(&raw)
	if !strings.HasPrefix(raw, "enc:v1:") {
		t.Errorf("a legacy row was not re-sealed on write: %q", raw)
	}
	if strings.Contains(raw, "legacy-value") {
		t.Errorf("the pre-existing key is still in the clear after a write: %q", raw)
	}
}

// This passes even without the explicit guard in SandboxConfigFor, because a
// sealed blob is not valid JSON; it does not prove the guard.
func TestSandboxConfigWithoutTheKeyYieldsNoEnvNotCiphertext(t *testing.T) {
	s, path := openConfigStore(t)
	if _, err := s.MergeSandboxConfig(t.Context(), "sb3", "org1",
		map[string]string{"ANTHROPIC_API_KEY": "sk-ant-secret"}, "", ""); err != nil {
		t.Fatalf("merge: %v", err)
	}
	s.Close()

	// Reopen with no key.
	s2, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	got := s2.SandboxConfigFor(t.Context(), "sb3")
	for k, v := range got.Env {
		if strings.HasPrefix(v, "enc:v1:") {
			t.Fatalf("%s would be injected into the guest as ciphertext: %q", k, v)
		}
	}
	if len(got.Env) != 0 {
		t.Errorf("expected no env when the value cannot be decrypted, got %v", got.Env)
	}
	_ = os.Remove(path)
}
