package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// OpenForTest opens a store of its own for one test: a fresh SQLite file, or,
// when SANDBOXD_TEST_POSTGRES_URL is set, a fresh schema in that database,
// dropped when the test ends. A schema rather than emptying shared tables: a
// server an earlier test left running keeps working in its own schema and
// cannot touch this test's rows.
func OpenForTest(t testing.TB) *Store {
	t.Helper()
	dsn := os.Getenv("SANDBOXD_TEST_POSTGRES_URL")
	if dsn == "" {
		s, err := Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("SANDBOXD_TEST_POSTGRES_URL: %v", err)
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	schema := "test_" + hex.EncodeToString(b)
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	if _, err := admin.ExecContext(t.Context(), `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		// Not t.Context(): it is already cancelled when cleanups run.
		_, _ = admin.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		_ = admin.Close()
	})
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	s, err := Open(t.Context(), u.String())
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// FailPreviewBumpsForTest makes every BumpPreviewGeneration on s fail until
// the returned func is called, so a caller's ordering around a failed bump
// can be tested against a real store. Reads are unaffected.
func FailPreviewBumpsForTest(t testing.TB, s *Store) (restore func()) {
	t.Helper()
	install := []string{
		`CREATE TRIGGER fail_preview_bump_ins BEFORE INSERT ON settings WHEN NEW.key LIKE 'preview_gen/%'
		 BEGIN SELECT RAISE(ABORT, 'injected preview bump failure'); END`,
		`CREATE TRIGGER fail_preview_bump_upd BEFORE UPDATE ON settings WHEN NEW.key LIKE 'preview_gen/%'
		 BEGIN SELECT RAISE(ABORT, 'injected preview bump failure'); END`,
	}
	remove := []string{`DROP TRIGGER fail_preview_bump_ins`, `DROP TRIGGER fail_preview_bump_upd`}
	if s.dialect == dialectPostgres {
		install = []string{
			`CREATE FUNCTION fail_preview_bump() RETURNS trigger AS $$
			 BEGIN RAISE EXCEPTION 'injected preview bump failure'; END $$ LANGUAGE plpgsql`,
			`CREATE TRIGGER fail_preview_bump BEFORE INSERT OR UPDATE ON settings
			 FOR EACH ROW WHEN (NEW.key LIKE 'preview_gen/%') EXECUTE FUNCTION fail_preview_bump()`,
		}
		remove = []string{`DROP TRIGGER fail_preview_bump ON settings`, `DROP FUNCTION fail_preview_bump()`}
	}
	for _, q := range install {
		if _, err := s.db.ExecContext(t.Context(), q); err != nil {
			t.Fatalf("install bump failure: %v", err)
		}
	}
	return func() {
		for _, q := range remove {
			if _, err := s.db.ExecContext(context.Background(), q); err != nil {
				t.Fatalf("remove bump failure: %v", err)
			}
		}
	}
}
