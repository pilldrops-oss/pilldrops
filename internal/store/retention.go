package store

import (
	"context"
	"fmt"
)

// RetentionTarget is one append-only table and the column it is pruned by.
// table and column are unexported because the prune concatenates them into
// SQL; only strings from this file may reach it.
type RetentionTarget struct {
	// Name is the env-facing knob: <NAME>_RETAIN_DAYS, uppercased.
	Name string
	// DefaultDays applies when nothing is configured. 0 means keep forever.
	DefaultDays int
	table       string
	column      string
	// extra is ANDed onto the range, for tables where 0 means "no timestamp".
	extra string
}

// RetentionTargets lists every pruned table. Defaults are generous: retention
// bounds growth, it is not a data-protection policy.
var RetentionTargets = []RetentionTarget{
	{
		// Longer than any window a usage query asks for, so windowed reads
		// never return a partial answer. Lifetime totals live in usage_totals.
		Name: "usage", DefaultDays: 400, table: "usage_records", column: "recorded_at",
	},
	{
		// at > 0: an event whose timestamp never arrived stores 0, and a range
		// delete would read that as 1970.
		Name: "agent_events", DefaultDays: 90, table: "agent_events", column: "at", extra: "at > 0",
	},
	{
		Name: "events", DefaultDays: 90, table: "sandbox_events", column: "at",
	},
	{
		Name: "history", DefaultDays: 30, table: "sandbox_history", column: "destroyed_at",
	},
	{
		// The delivery log is also the dead-letter queue, so this prune
		// deletes something an operator might still act on.
		Name: "webhook_deliveries", DefaultDays: 30, table: "agent_webhook_deliveries", column: "created_at",
	},
	{
		Name: "idempotency", DefaultDays: 7, table: "idempotency", column: "at",
	},
	{
		Name: "launch_timings", DefaultDays: 90, table: "launch_timings", column: "at",
	},
	{
		Name: "login_events", DefaultDays: 365, table: "login_events", column: "at",
	},
	{
		Name: "build_requests", DefaultDays: 365, table: "build_requests", column: "at",
	},
	{
		Name: "audit", DefaultDays: 365, table: "audit_log", column: "at",
	},
}

// PruneOlderThan deletes one target's rows older than a unix timestamp and
// returns the count.
func (s *Store) PruneOlderThan(ctx context.Context, t RetentionTarget, before int64) (int64, error) {
	q := fmt.Sprintf(`DELETE FROM %s WHERE %s < ?`, t.table, t.column)
	if t.extra != "" {
		q += " AND " + t.extra
	}
	res, err := s.exec(ctx, q, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
