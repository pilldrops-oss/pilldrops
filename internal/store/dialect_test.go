package store

import (
	"os"
	"testing"
)

func TestRebind_Postgres(t *testing.T) {
	s := &Store{dialect: dialectPostgres}
	got := s.rebind("INSERT INTO t (a, b, c) VALUES (?, ?, ?)")
	want := "INSERT INTO t (a, b, c) VALUES ($1, $2, $3)"
	if got != want {
		t.Fatalf("rebind postgres:\n got  %q\n want %q", got, want)
	}
}

func TestRebind_SQLiteUnchanged(t *testing.T) {
	s := &Store{dialect: dialectSQLite}
	q := "UPDATE t SET a = ? WHERE id = ?"
	if got := s.rebind(q); got != q {
		t.Fatalf("rebind sqlite must be a no-op: got %q", got)
	}
}

func TestRebind_ManyPlaceholders(t *testing.T) {
	s := &Store{dialect: dialectPostgres}
	got := s.rebind("VALUES (?, ?, ?, ?, ?, ?, ?)")
	want := "VALUES ($1, $2, $3, $4, $5, $6, $7)"
	if got != want {
		t.Fatalf("rebind 7:\n got  %q\n want %q", got, want)
	}
}

func TestRebind_NoPlaceholders(t *testing.T) {
	s := &Store{dialect: dialectPostgres}
	q := "SELECT 1"
	if got := s.rebind(q); got != q {
		t.Fatalf("rebind with no placeholders should return input unchanged: got %q", got)
	}
}

// TestOpenPostgres_Schema is skipped unless SANDBOXD_TEST_POSTGRES_URL is set.
func TestOpenPostgres_Schema(t *testing.T) {
	dsn := os.Getenv("SANDBOXD_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set SANDBOXD_TEST_POSTGRES_URL to run Postgres tests")
	}
	s, err := Open(t.Context(), dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer s.Close()
	if s.dialect != dialectPostgres {
		t.Fatalf("expected postgres dialect, got %v", s.dialect)
	}
	for _, table := range []string{"api_keys", "sandboxes", "usage_records", "functions", "org_plans", "settings"} {
		if _, err := s.db.Exec("SELECT count(*) FROM " + table); err != nil {
			t.Fatalf("table %s missing on postgres: %v", table, err)
		}
	}
}
