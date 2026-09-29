package store

import (
	"path/filepath"
	"testing"
)

func vaultStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.Context(), filepath.Join(t.TempDir(), "v.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.SetSecretKey("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatalf("secret key: %v", err)
	}
	return s
}

func TestSecretRoundTrips(t *testing.T) {
	s := vaultStore(t)
	ctx := t.Context()

	got, err := s.PutSecret(ctx, "u:acme", "ANTHROPIC_API_KEY", "sk-ant-secret-abcd", true, false)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if got.Last4 != "abcd" {
		t.Errorf("last4 = %q, want abcd", got.Last4)
	}
	if got.Value != "" {
		t.Errorf("Put returned the credential: %q", got.Value)
	}

	val, ok, err := s.SecretValue(ctx, "u:acme", "ANTHROPIC_API_KEY")
	if err != nil || !ok {
		t.Fatalf("value: %v ok=%v", err, ok)
	}
	if val != "sk-ant-secret-abcd" {
		t.Errorf("round trip = %q", val)
	}
}

func TestSecretIsEncryptedAtRest(t *testing.T) {
	s := vaultStore(t)
	if _, err := s.PutSecret(t.Context(), "u:acme", "OPENAI_API_KEY", "sk-plaintext-xyz", true, false); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := s.db.QueryRowContext(t.Context(),
		`SELECT value FROM vault_secrets WHERE org_id = ? AND name = ?`,
		"u:acme", "OPENAI_API_KEY").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw == "sk-plaintext-xyz" {
		t.Fatal("the secret is stored in plaintext")
	}
	if !hasEncPrefix(raw) {
		t.Errorf("stored value %q carries no encryption marker", raw)
	}
}

func hasEncPrefix(s string) bool { return len(s) > 7 && s[:7] == encPrefix }

func TestEnvKindIsReadableAndSecretIsSealed(t *testing.T) {
	s := vaultStore(t)
	ctx := t.Context()
	if _, err := s.PutSecret(ctx, "u:acme", "BAR", "sk-secret-wxyz", true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutSecret(ctx, "u:acme", "FOO", "https://gw.example/v1", false, true); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListSecrets(ctx, "u:acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("list = %d rows", len(list))
	}
	for _, v := range list {
		switch v.Name {
		case "BAR":
			if v.Value != "" || v.Last4 != "wxyz" || v.Kind != VaultKindSecret || v.AutoInject {
				t.Errorf("sealed secret listed as %+v", v)
			}
		case "FOO":
			if v.Value != "https://gw.example/v1" || v.Kind != VaultKindEnv || !v.AutoInject {
				t.Errorf("env kind listed as %+v — a setting you cannot verify is a typo you cannot find", v)
			}
		}
	}
	auto, err := s.ListAutoInject(ctx, "u:acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(auto) != 1 || auto["FOO"] != "https://gw.example/v1" {
		t.Errorf("auto-inject = %v", auto)
	}
	if other, _ := s.ListAutoInject(ctx, "u:other"); len(other) != 0 {
		t.Errorf("another org sees auto-inject entries: %v", other)
	}
}

// The legacy readable-settings map became 'env' rows in migration 52: a
// pre-51 row named OPENAI_MODEL must list with its value after migrate.
func TestMigrationMarksLegacyReadableSettingsAsEnv(t *testing.T) {
	s := vaultStore(t)
	ctx := t.Context()
	if _, err := s.PutSecret(ctx, "u:acme", "OPENAI_MODEL", "gpt-4.1", true, false); err != nil {
		t.Fatal(err)
	}
	// Rewind the ledger so 51 re-runs against a row that is still 'secret'.
	if _, err := s.exec(ctx, `DELETE FROM schema_migrations WHERE version = 52`); err != nil {
		t.Fatal(err)
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListSecrets(ctx, "u:acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Kind != VaultKindEnv || list[0].Value != "gpt-4.1" {
		t.Errorf("legacy readable setting after migration: %+v", list)
	}
}

func TestSecretsAreOrgScoped(t *testing.T) {
	s := vaultStore(t)
	ctx := t.Context()
	if _, err := s.PutSecret(ctx, "u:alice", "ANTHROPIC_API_KEY", "alice-key", true, false); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.SecretValue(ctx, "u:bob", "ANTHROPIC_API_KEY"); ok {
		t.Error("bob read alice's secret")
	}
	if list, _ := s.ListSecrets(ctx, "u:bob"); len(list) != 0 {
		t.Errorf("bob's list has %d of alice's secrets", len(list))
	}
	if deleted, _ := s.DeleteSecret(ctx, "u:bob", "ANTHROPIC_API_KEY"); deleted {
		t.Error("bob deleted alice's secret")
	}
	if _, ok, _ := s.SecretValue(ctx, "u:alice", "ANTHROPIC_API_KEY"); !ok {
		t.Error("alice's secret is gone after bob's delete")
	}
}

func TestSecretNameMustBeAnEnvIdentifier(t *testing.T) {
	s := vaultStore(t)
	for _, bad := range []string{"HAS SPACE", "HAS-DASH", "1LEADING", "", "A;rm -rf /", "PD_X", "PILLDROPS_Y"} {
		if _, err := s.PutSecret(t.Context(), "u:acme", bad, "v", true, false); err == nil {
			t.Errorf("accepted %q as a secret name — it becomes an env var", bad)
		}
	}
	if _, err := s.PutSecret(t.Context(), "u:acme", "lower_case", "v", true, false); err != nil {
		t.Errorf("lower_case refused: %v", err)
	}
}

func TestPutReplacesAndDeleteReports(t *testing.T) {
	s := vaultStore(t)
	ctx := t.Context()
	if _, err := s.PutSecret(ctx, "u:acme", "OPENAI_API_KEY", "old-value-1111", true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutSecret(ctx, "u:acme", "OPENAI_API_KEY", "new-value-2222", true, false); err != nil {
		t.Fatal(err)
	}
	val, _, _ := s.SecretValue(ctx, "u:acme", "OPENAI_API_KEY")
	if val != "new-value-2222" {
		t.Errorf("replace left %q", val)
	}
	if list, _ := s.ListSecrets(ctx, "u:acme"); len(list) != 1 {
		t.Errorf("replace created a duplicate: %d rows", len(list))
	}
	if deleted, _ := s.DeleteSecret(ctx, "u:acme", "OPENAI_API_KEY"); !deleted {
		t.Error("delete reported nothing removed")
	}
	if deleted, _ := s.DeleteSecret(ctx, "u:acme", "OPENAI_API_KEY"); deleted {
		t.Error("second delete reported a removal")
	}
}
