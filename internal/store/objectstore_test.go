package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestObjectStoreRoundTripsSealed(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	want := ObjectStore{Endpoint: "https://s3.example.com", Bucket: "b", Region: "eu-west-3", AccessKey: "AKIAEXAMPLE", SecretKey: "s3cr3t-value"}

	if err := st.SetObjectStore(ctx, want); !errors.Is(err, ErrObjectStoreNeedsKey) {
		t.Fatalf("save without a secret key = %v, want ErrObjectStoreNeedsKey", err)
	}
	if err := st.SetSecretKey(strings.Repeat("ab", 32)); err != nil {
		t.Fatalf("secret key: %v", err)
	}
	if got, err := st.GetObjectStore(ctx); got != nil || err != nil {
		t.Fatalf("unset = %v, %v, want nil", got, err)
	}
	if err := st.SetObjectStore(ctx, want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := st.GetObjectStore(ctx)
	if err != nil || got == nil || *got != want {
		t.Fatalf("get = %v, %v, want what was saved", got, err)
	}
	var raw string
	if err := st.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = $1`, objectStoreKey).Scan(&raw); err != nil {
		t.Fatalf("raw read: %v", err)
	}
	if strings.Contains(raw, want.SecretKey) || !strings.HasPrefix(raw, encPrefix) {
		t.Fatalf("row is not sealed: %q", raw)
	}
	if err := st.DeleteObjectStore(ctx); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got, err := st.GetObjectStore(ctx); got != nil || err != nil {
		t.Fatalf("after delete = %v, %v, want nil", got, err)
	}
}
