package store

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// Postgres only: SQLite is serialized by SetMaxOpenConns(1), so the race
// cannot happen there.
func TestConcurrentOpenMigratesExactlyOnce(t *testing.T) {
	dsn := os.Getenv("SANDBOXD_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set SANDBOXD_TEST_POSTGRES_URL to run Postgres tests")
	}
	// Needs a cold database; against a migrated one the race never opens.
	admin, err := Open(t.Context(), dsn)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	dbName := fmt.Sprintf("migrace_%d", time.Now().UnixNano())
	if _, err := admin.db.Exec(`CREATE DATABASE ` + dbName); err != nil {
		admin.Close()
		t.Skipf("cannot create a scratch database (%v) — needs a superuser DSN", err)
	}
	admin.Close()
	coldDSN := swapDBName(dsn, dbName)

	const replicas = 8
	var start, done sync.WaitGroup
	start.Add(1)
	errs := make([]error, replicas)
	for i := 0; i < replicas; i++ {
		done.Add(1)
		go func(i int) {
			defer done.Done()
			start.Wait()
			s, err := Open(t.Context(), coldDSN)
			if err != nil {
				errs[i] = err
				return
			}
			s.Close()
		}(i)
	}
	start.Done()
	done.Wait()

	failed := 0
	for i, err := range errs {
		if err != nil {
			failed++
			t.Errorf("replica %d could not open the store: %v", i, err)
		}
	}
	if failed > 0 {
		t.Errorf("%d/%d simultaneous opens failed. On the orchestrator that is not a crash — "+
			"it boots WITHOUT a store, and admission allows everything when the store is nil",
			failed, replicas)
	}

	// The schema must be complete, not merely recorded as complete.
	s, err := Open(t.Context(), coldDSN)
	if err != nil {
		t.Fatalf("final open: %v", err)
	}
	defer s.Close()
	for _, col := range []struct{ table, column string }{
		{"sandboxes", "client"},
		{"sandbox_history", "client"},
		{"sandboxes", "public_preview"},
		{"vault_secrets", "value"},
	} {
		var n int
		if err := s.db.QueryRow(
			`SELECT COUNT(*) FROM information_schema.columns WHERE table_name=$1 AND column_name=$2`,
			col.table, col.column).Scan(&n); err != nil {
			t.Fatalf("introspect %s.%s: %v", col.table, col.column, err)
		}
		if n != 1 {
			t.Errorf("%s.%s missing after a concurrent migrate — the schema is behind what "+
				"schema_migrations claims, which is worse than failing outright", col.table, col.column)
		}
	}
}

// swapDBName rewrites the database segment of a postgres DSN.
func swapDBName(dsn, name string) string {
	i := len(dsn)
	if q := indexByte(dsn, '?'); q >= 0 {
		i = q
	}
	head, tail := dsn[:i], dsn[i:]
	if sl := lastIndexByte(head, '/'); sl >= 0 {
		head = head[:sl+1] + name
	}
	return head + tail
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func lastIndexByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}
