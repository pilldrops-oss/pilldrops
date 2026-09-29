package store

import (
	"testing"
)

// TestApiKeysOrgIndex verifies the ListKeys query has a supporting index.
func TestApiKeysOrgIndex(t *testing.T) {
	st := openTemp(t)

	q := `SELECT name FROM sqlite_master WHERE type='index' AND name='idx_keys_org'`
	if st.dialect == dialectPostgres {
		q = `SELECT indexname AS name FROM pg_indexes WHERE indexname = 'idx_keys_org'`
	}
	var name string
	if err := st.queryRow(t.Context(), q).Scan(&name); err != nil || name != "idx_keys_org" {
		t.Fatalf("idx_keys_org missing (err=%v name=%q)", err, name)
	}

	if _, _, err := st.CreateKey(t.Context(), "k", "org1"); err != nil {
		t.Fatalf("create key: %v", err)
	}
	keys, err := st.ListKeys(t.Context(), "org1")
	if err != nil || len(keys) != 1 {
		t.Fatalf("ListKeys org1: err=%v len=%d", err, len(keys))
	}
}
