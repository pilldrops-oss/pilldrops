package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.Context(), filepath.Join(t.TempDir(), "v.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func rawVaultValue(t *testing.T, st *Store, org string) string {
	t.Helper()
	var raw string
	if err := st.db.QueryRowContext(context.Background(),
		`SELECT value FROM vault_secrets WHERE org_id=$1`, org).Scan(&raw); err != nil {
		t.Fatalf("read raw: %v", err)
	}
	return raw
}

func TestPutSecretRefusesToStorePlaintext(t *testing.T) {
	st := openTestStore(t) // no key configured
	_, err := st.PutSecret(t.Context(), "u:acme", "ANTHROPIC_API_KEY", "sk-ant-SUPERSECRET", true, false)
	if err == nil {
		t.Fatalf("PutSecret stored a provider key with no encryption configured; on-disk value = %q",
			rawVaultValue(t, st, "u:acme"))
	}
	if !errors.Is(err, errPlaintextVault) {
		t.Fatalf("err = %v, want errPlaintextVault", err)
	}
}

func TestVaultKeyEncryptsAndRoundTrips(t *testing.T) {
	st := openTestStore(t)
	const key = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	if err := st.SetVaultKey(key); err != nil {
		t.Fatalf("SetVaultKey: %v", err)
	}
	if _, err := st.PutSecret(t.Context(), "u:acme", "ANTHROPIC_API_KEY", "sk-ant-SUPERSECRET", true, false); err != nil {
		t.Fatalf("PutSecret: %v", err)
	}
	if raw := rawVaultValue(t, st, "u:acme"); strings.Contains(raw, "SUPERSECRET") {
		t.Fatalf("value stored in the clear: %q", raw)
	}
	got, ok, err := st.SecretValue(t.Context(), "u:acme", "ANTHROPIC_API_KEY")
	if err != nil || !ok || got != "sk-ant-SUPERSECRET" {
		t.Fatalf("SecretValue = (%q, %v, %v), want the original", got, ok, err)
	}
}

func TestTheGeneralKeyCannotOpenTheVault(t *testing.T) {
	const general = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const vaultK = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	cp := openTestStore(t) // the control plane: both keys
	if err := cp.SetSecretKey(general); err != nil {
		t.Fatal(err)
	}
	if err := cp.SetVaultKey(vaultK); err != nil {
		t.Fatal(err)
	}
	if _, err := cp.PutSecret(t.Context(), "u:acme", "ANTHROPIC_API_KEY", "sk-ant-SUPERSECRET", true, false); err != nil {
		t.Fatalf("PutSecret: %v", err)
	}
	sealed := rawVaultValue(t, cp, "u:acme")

	node := openTestStore(t)
	if err := node.SetSecretKey(general); err != nil {
		t.Fatal(err)
	}
	if plain, err := node.crypt.open(sealed); err == nil && strings.Contains(plain, "SUPERSECRET") {
		t.Fatal("a node's SANDBOXD_SECRET_KEY decrypted a vault row sealed with VAULT_SECRET_KEY")
	}
}

// Rows sealed with the general key must still open after the vault key is introduced.
func TestPreSplitRowsStillOpen(t *testing.T) {
	const general = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const vaultK = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	st := openTestStore(t)
	if err := st.SetSecretKey(general); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutSecret(t.Context(), "u:acme", "OPENAI_API_KEY", "sk-old-value", true, false); err != nil {
		t.Fatalf("PutSecret: %v", err)
	}
	if err := st.SetVaultKey(vaultK); err != nil {
		t.Fatal(err)
	}
	got, ok, err := st.SecretValue(t.Context(), "u:acme", "OPENAI_API_KEY")
	if err != nil || !ok || got != "sk-old-value" {
		t.Fatalf("SecretValue after introducing the vault key = (%q, %v, %v), want the pre-split value",
			got, ok, err)
	}
}
