package store

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// splitSQL splits a multi-statement DDL string on ';'. The schema DDL has no
// ';' inside literals, so a plain split is safe.
func splitSQL(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ";") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// pendingMigrations returns the migrations not yet applied, in version order.
func pendingMigrations(applied map[int]bool, all []migration) []migration {
	var out []migration
	for _, m := range all {
		if !applied[m.version] {
			out = append(out, m)
		}
	}
	return out
}

// isDuplicateObjectErr reports whether err is a benign "already exists" from
// re-running idempotent DDL, or a duplicate-key error from a concurrent migrator
// that already did identical work.
func isDuplicateObjectErr(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate column") ||
		strings.Contains(msg, "already exists") ||
		strings.Contains(msg, "duplicate key value")
}

// isMissingObjectErr reports whether err says the object to remove is already
// gone. Honoured only for migrations marked objectsMayBeGone.
func isMissingObjectErr(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such table") ||
		strings.Contains(msg, "no such column") ||
		strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "undefined table") ||
		strings.Contains(msg, "undefined column")
}

// migrateLockKey names the Postgres advisory lock that serializes migrators.
// Spelled out so an operator can find it in pg_locks.
const migrateLockKey = 0x5A4D4947 // "ZMIG"

// migrate runs pending migrations in order and records them in
// schema_migrations. Idempotent, so it runs on every Open.
func (s *Store) migrate(ctx context.Context) error {
	// One migrator at a time: CREATE TABLE IF NOT EXISTS races in Postgres.
	// The session lock lives on a dedicated conn so the pool cannot hand the
	// unlock to another session. SQLite is serialized by SetMaxOpenConns(1).
	if s.dialect == dialectPostgres {
		conn, err := s.db.Conn(ctx)
		if err != nil {
			return fmt.Errorf("migrate lock: %w", err)
		}
		defer func() {
			// Best-effort; closing the connection releases the lock anyway.
			_, _ = conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, migrateLockKey)
			_ = conn.Close()
		}()
		if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, migrateLockKey); err != nil {
			return fmt.Errorf("migrate lock: %w", err)
		}
	}

	// Read first and create the ledger only if that fails. Postgres ACL-checks
	// the schema before IF NOT EXISTS can short-circuit, so an unconditional
	// CREATE fails for the node role, which has no CREATE on schema public.
	applied := map[int]bool{}
	appliedName := map[int]string{}
	rows, err := s.query(ctx, `SELECT version, name FROM schema_migrations`)
	if err != nil {
		if _, cerr := s.exec(ctx,
			`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at INTEGER NOT NULL)`,
		); cerr != nil {
			return fmt.Errorf("schema_migrations is unreadable (%w) and could not be created: %w", err, cerr)
		}
		if rows, err = s.query(ctx, `SELECT version, name FROM schema_migrations`); err != nil {
			return fmt.Errorf("read schema_migrations: %w", err)
		}
	}
	defer func() { _ = rows.Close() }() //nolint:errcheck // best-effort cleanup
	for rows.Next() {
		var v int
		var n string
		if err := rows.Scan(&v, &n); err != nil {
			return err
		}
		applied[v] = true
		appliedName[v] = n
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		// A truncated read would make an applied migration look unapplied and re-run it.
		return fmt.Errorf("read schema_migrations: %w", err)
	}

	// A version recorded under a different name means two branches reused the
	// number, so this build's migration silently never runs. Log loudly but do
	// not refuse to boot; the fix is re-adding the objects under a new version.
	for _, m := range migrations {
		if was, ok := appliedName[m.version]; ok && was != m.name {
			slog.Error("MIGRATION VERSION REUSED: this version is recorded under a different name, "+
				"so this build's migration never ran here; anything it was to add is missing "+
				"until it is re-added under a new version",
				"component", "store", "version", m.version, "recorded_as", was, "this_build", m.name)
		}
	}

	for _, m := range pendingMigrations(applied, migrations) {
		stmts := m.sqlite
		if s.dialect == dialectPostgres {
			stmts = m.postgres
		}
		for _, stmt := range stmts {
			if _, err := s.exec(ctx, stmt); err != nil &&
				!isDuplicateObjectErr(err) &&
				(!m.objectsMayBeGone || !isMissingObjectErr(err)) {
				return fmt.Errorf("migration %d (%s): %w", m.version, m.name, err)
			}
		}
		if _, err := s.exec(ctx,
			`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
			m.version, m.name, time.Now().Unix(),
		); err != nil && !isDuplicateObjectErr(err) {
			return fmt.Errorf("record migration %d: %w", m.version, err)
		}
	}
	return nil
}
