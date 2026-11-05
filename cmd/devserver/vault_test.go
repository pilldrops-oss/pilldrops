package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testVault(t *testing.T) *VaultStore {
	t.Helper()
	return LoadVault(filepath.Join(t.TempDir(), "vault.json"), "test-material")
}

func TestVaultRoundTrip(t *testing.T) {
	vs := testVault(t)
	if _, err := vs.Put("u:alice", "ANTHROPIC_API_KEY", "sk-ant-secret-1234", true, false); err != nil {
		t.Fatal(err)
	}
	got, ok := vs.Value("u:alice", "ANTHROPIC_API_KEY")
	if !ok || got != "sk-ant-secret-1234" {
		t.Fatalf("Value = %q, %v", got, ok)
	}
	// Tenancy: another owner must see nothing.
	if _, ok := vs.Value("u:bob", "ANTHROPIC_API_KEY"); ok {
		t.Fatal("cross-tenant read succeeded")
	}
	if got := vs.List("u:bob"); len(got) != 0 {
		t.Fatalf("bob sees %d secrets", len(got))
	}
	// List never exposes value, ciphertext or any part of the value.
	list := vs.List("u:alice")
	if len(list) != 1 || list[0].Cipher != "" || list[0].Owner != "" {
		t.Fatalf("List leak/shape: %+v", list[0])
	}
}

func TestVaultPersistsEncrypted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vault.json")
	vs := LoadVault(path, "material")
	if _, err := vs.Put("u:alice", "OPENAI_API_KEY", "sk-plaintext-value", true, false); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sk-plaintext-value") {
		t.Fatal("plaintext secret on disk")
	}
	// Reload with the same material decrypts.
	if got, ok := LoadVault(path, "material").Value("u:alice", "OPENAI_API_KEY"); !ok || got != "sk-plaintext-value" {
		t.Fatalf("reload Value = %q, %v", got, ok)
	}
	// Reload with rotated material is absent, never garbage.
	if _, ok := LoadVault(path, "rotated").Value("u:alice", "OPENAI_API_KEY"); ok {
		t.Fatal("rotated key still decrypts")
	}
}

func TestVaultValidation(t *testing.T) {
	vs := testVault(t)
	for _, bad := range []struct{ owner, name, value string }{
		{"", "A_B", "v"},         // no owner
		{"anon", "A_B", "v"},     // anonymous
		{"u:alice", "1BAD", "v"}, // not an env identifier
		{"u:alice", "PD_X", "v"}, // reserved platform prefix
		{"u:alice", "A;rm", "v"}, // shell metacharacters
		{"u:alice", "A_B", ""},   // empty value
	} {
		if _, err := vs.Put(bad.owner, bad.name, bad.value, true, false); err == nil {
			t.Errorf("Put(%q,%q,%q) accepted", bad.owner, bad.name, bad.value)
		}
	}
}

func TestResolveSecretRefs(t *testing.T) {
	vs := testVault(t)
	if _, err := vs.Put("u:alice", "ANTHROPIC_API_KEY", "sk-live-key", true, false); err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"template":"node","env":["FOO=bar"],"secret_refs":["ANTHROPIC_API_KEY"]}`)
	out, err := resolveSecretRefs(body, vs, "u:alice")
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if _, has := obj["secret_refs"]; has {
		t.Fatal("secret_refs not stripped before forward")
	}
	env, _ := obj["env"].([]any)
	if len(env) != 2 || env[1] != "ANTHROPIC_API_KEY=sk-live-key" {
		t.Fatalf("env = %v", env)
	}

	// Unknown ref fails loudly (a silently key-less agent is worse).
	if _, err := resolveSecretRefs([]byte(`{"secret_refs":["MISSING"]}`), vs, "u:alice"); err == nil {
		t.Fatal("missing ref did not error")
	}
	// Another tenant's ref is a missing ref.
	if _, err := resolveSecretRefs(body, vs, "u:bob"); err == nil {
		t.Fatal("cross-tenant ref resolved")
	}
	// No refs: body untouched byte-for-byte.
	plain := []byte(`{"template":"base"}`)
	if out, _ := resolveSecretRefs(plain, vs, "u:alice"); string(out) != string(plain) {
		t.Fatalf("ref-less body rewritten: %s", out)
	}
}
