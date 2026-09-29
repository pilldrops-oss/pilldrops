package store

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/observability"
	"go.opentelemetry.io/otel/attribute"
)

// dialect selects SQL dialect behavior for the open backend.
type dialect int

const (
	dialectSQLite dialect = iota
	dialectPostgres
)

// rebind rewrites `?` placeholders into `$1, $2, ...` for Postgres. Queries
// must never carry a literal `?` inside a string.
func (s *Store) rebind(q string) string {
	if s.dialect != dialectPostgres {
		return q
	}
	if !strings.Contains(q, "?") {
		return q
	}
	var b strings.Builder
	b.Grow(len(q) + 8)
	n := 0
	for i := 0; i < len(q); i++ {
		if q[i] == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(q[i])
	}
	return b.String()
}

// exec, query and queryRow are the dialect-aware entry points every query file
// uses instead of s.db.* directly. exec applies stmtTimeout because its Result
// completes before it returns.
func (s *Store) exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	ctx, span := observability.StartInternal(ctx, "db.exec", attribute.String("db.operation", sqlVerb(q)))
	defer span.End()
	ctx, cancel := s.stmtCtx(ctx)
	defer cancel()
	res, err := s.db.ExecContext(ctx, s.rebind(q), args...)
	if err != nil {
		span.RecordError(err)
	}
	return res, err
}

// query and queryRow pass the caller's context through without a timeout: the
// returned rows outlive this call, and cancelling a derived context on return
// would kill the result set. The read deadline belongs to the caller.
func (s *Store) query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	// The span covers issuing the query, not iterating it.
	ctx, span := observability.StartInternal(ctx, "db.query", attribute.String("db.operation", sqlVerb(q)))
	defer span.End()
	rows, err := s.db.QueryContext(ctx, s.rebind(q), args...) //nolint:sqlclosecheck // the caller owns and closes the rows; see the note above
	if err != nil {
		span.RecordError(err)
	}
	return rows, err
}

func (s *Store) queryRow(ctx context.Context, q string, args ...any) *sql.Row {
	ctx, span := observability.StartInternal(ctx, "db.queryRow", attribute.String("db.operation", sqlVerb(q)))
	defer span.End()
	return s.db.QueryRowContext(ctx, s.rebind(q), args...)
}

// sqlVerb extracts the leading keyword so spans group by operation without
// carrying customer identifiers.
func sqlVerb(q string) string {
	q = strings.TrimSpace(q)
	if i := strings.IndexAny(q, " \t\n"); i > 0 {
		return strings.ToUpper(q[:i])
	}
	return "UNKNOWN"
}

// stmtTimeout is the ceiling on a single write, even for callers with no deadline.
const stmtTimeout = 30 * time.Second

// stmtCtx derives the effective context for one statement, applying stmtTimeout
// unless the caller's own deadline is already sooner.
func (s *Store) stmtCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) <= stmtTimeout {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, stmtTimeout)
}

// collectRows runs a query and builds one T per row. It is how every list
// query in this package reads its rows: the loop, the close, the per-row error
// and rows.Err() are the same five lines every time, and forgetting the last
// one turns a truncated read into a short answer nobody notices.
func collectRows[T any](ctx context.Context, s *Store, q string, args []any, scan func(*sql.Rows, *T) error) ([]T, error) {
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Empty, not nil: the result is marshaled as a JSON array.
	out := []T{}
	for rows.Next() {
		var v T
		if err := scan(rows, &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// scanInto adapts a scanX(row) (T, error) helper -- the form a single-row
// lookup needs -- to the form collectRows fills.
func scanInto[T any](scan func(rowScanner) (T, error)) func(*sql.Rows, *T) error {
	return func(rows *sql.Rows, v *T) error {
		var err error
		*v, err = scan(rows)
		return err
	}
}
