package store

import "testing"

// Old rows scope to "all", which injects exactly where they injected before.
func TestMigrationV61LeavesExistingSecretsUnscoped(t *testing.T) {
	st := openTemp(t)
	st.SetAllowPlaintextVault(true)
	if _, err := st.exec(t.Context(),
		`INSERT INTO vault_secrets (org_id, name, value, last4, kind, auto_inject, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"u:cust1", "OLD_KEY", "sk-legacy", "gacy", VaultKindSecret, 1, int64(1), int64(1),
	); err != nil {
		t.Fatalf("insert a pre-scope row: %v", err)
	}
	_, scope, createdBy, found, err := st.SecretForInject(t.Context(), "u:cust1", "OLD_KEY")
	if err != nil || !found {
		t.Fatalf("SecretForInject: found=%v err=%v", found, err)
	}
	if scope != ScopeAll || createdBy != "" {
		t.Fatalf("pre-scope row reads (scope=%q, created_by=%q), want (all, \"\")", scope, createdBy)
	}
	// Still auto-injected, because "all" is what it always was.
	auto, err := st.ListAutoInject(t.Context(), "u:cust1")
	if err != nil || auto["OLD_KEY"] != "sk-legacy" {
		t.Fatalf("auto-inject = %v, err=%v; an existing auto entry must keep injecting", auto, err)
	}
}

// Narrowing a secret's scope must not disturb the value: nobody should have to
// re-type a credential to decide where it may go.
func TestSetSecretScopeKeepsTheValue(t *testing.T) {
	st := openTemp(t)
	st.SetAllowPlaintextVault(true)
	before, err := st.PutSecret(t.Context(), "u:cust1", "API_KEY", "sk-secret-value", true, true, "key_alice")
	if err != nil {
		t.Fatalf("PutSecret: %v", err)
	}
	if before.CreatedBy != "key_alice" || before.Scope != ScopeAll {
		t.Fatalf("new secret = (created_by=%q, scope=%q), want (key_alice, all)", before.CreatedBy, before.Scope)
	}

	ok, err := st.SetSecretScope(t.Context(), "u:cust1", "API_KEY", ScopePrivate)
	if err != nil || !ok {
		t.Fatalf("SetSecretScope: ok=%v err=%v", ok, err)
	}
	val, scope, owner, _, err := st.SecretForInject(t.Context(), "u:cust1", "API_KEY")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if val != "sk-secret-value" || scope != ScopePrivate || owner != "key_alice" {
		t.Fatalf("after the scope change: value=%q scope=%q owner=%q", val, scope, owner)
	}
	list, err := st.ListSecrets(t.Context(), "u:cust1")
	if err != nil || len(list) != 1 || list[0].Scope != ScopePrivate {
		t.Fatalf("list = %+v (err %v), want the new scope", list, err)
	}
	// A scoped secret stops auto-injecting: "every sandbox" cannot honour a scope.
	auto, err := st.ListAutoInject(t.Context(), "u:cust1")
	if err != nil || len(auto) != 0 {
		t.Fatalf("auto-inject = %v, want none once the secret is scoped", auto)
	}

	if _, err := st.SetSecretScope(t.Context(), "u:cust1", "API_KEY", "nonsense"); err == nil {
		t.Error("an unknown scope was accepted")
	}
	if ok, _ := st.SetSecretScope(t.Context(), "u:cust1", "NO_SUCH", ScopeAll); ok {
		t.Error("scoping a secret that does not exist reported success")
	}
}
