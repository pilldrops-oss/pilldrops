package store

import "testing"

func TestEnsureKeyWithValue(t *testing.T) {
	st := openTemp(t)

	if err := st.EnsureKeyWithValue(t.Context(), "sf_node_abc123", "default", "operator"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := st.EnsureKeyWithValue(t.Context(), "sf_node_abc123", "default", "operator"); err != nil {
		t.Fatalf("ensure (2nd): %v", err)
	}

	k, err := st.ValidateKey(t.Context(), "sf_node_abc123")
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if k.Role != "operator" || k.OrgID != "default" {
		t.Errorf("key = %+v, want operator/default", k)
	}
}
