package nodeboot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

func TestBootstrapKeyIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	BootstrapKey(t.Context(), st, dir)
	first, err := os.ReadFile(filepath.Join(dir, "bootstrap.key"))
	if err != nil {
		t.Fatalf("first boot wrote no key: %v", err)
	}
	BootstrapKey(t.Context(), st, dir)
	second, err := os.ReadFile(filepath.Join(dir, "bootstrap.key"))
	if err != nil {
		t.Fatalf("key file gone after second boot: %v", err)
	}
	if string(first) != string(second) {
		t.Error("second BootstrapKey call overwrote bootstrap.key — marker did not short-circuit")
	}
}

// The key must land in a 0600 file, never on stdout where log capture would see it.
func TestBootstrapKeyWritesOperatorKeyToFile(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	BootstrapKey(t.Context(), st, dir)

	path := filepath.Join(dir, "bootstrap.key")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("bootstrap.key not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("bootstrap.key perm = %v, want 0600", perm)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read bootstrap.key: %v", err)
	}
	key := strings.TrimSpace(string(data))
	k, err := st.ValidateKey(t.Context(), key)
	if err != nil {
		t.Fatalf("bootstrap key does not validate: %v", err)
	}
	if k.Role != "operator" {
		t.Errorf("bootstrap key role = %q, want operator", k.Role)
	}
}
