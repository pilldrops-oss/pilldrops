package store

import (
	"testing"
)

func TestKeyRoleRoundTrip(t *testing.T) {
	st := openTemp(t)

	_, userKey, err := st.CreateKey(t.Context(), "u", "default")
	if err != nil {
		t.Fatalf("create user key: %v", err)
	}
	k, err := st.ValidateKey(t.Context(), userKey)
	if err != nil {
		t.Fatalf("validate user: %v", err)
	}
	if k.Role != "user" {
		t.Errorf("default role = %q, want user", k.Role)
	}

	_, opKey, err := st.CreateKeyWithRole(t.Context(), "op", "default", "operator")
	if err != nil {
		t.Fatalf("create operator key: %v", err)
	}
	k2, err := st.ValidateKey(t.Context(), opKey)
	if err != nil {
		t.Fatalf("validate operator: %v", err)
	}
	if k2.Role != "operator" {
		t.Errorf("role = %q, want operator", k2.Role)
	}
}
