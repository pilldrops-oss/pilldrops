package store

import (
	"context"
	"database/sql"
)

// LoginEvent is one recorded sign-in.
type LoginEvent struct {
	OwnerID string `json:"owner_id"`
	Email   string `json:"email"`
	At      int64  `json:"at"`
}

// RecordLoginEvent appends one login to login_events; empty owners are ignored.
func (s *Store) RecordLoginEvent(ctx context.Context, at int64, ownerID, email string) error {
	if ownerID == "" {
		return nil
	}
	_, err := s.exec(ctx, `INSERT INTO login_events (owner_id, email, at) VALUES (?, ?, ?)`, ownerID, email, at)
	return err
}

// AdminSessions is the operator session/login view.
type AdminSessions struct {
	Total       int64        `json:"total"`
	Logins24h   int64        `json:"logins_24h"`
	Logins7d    int64        `json:"logins_7d"`
	UniqueUsers int64        `json:"unique_users"`
	ByDay       []DayCount   `json:"by_day"` // day = unix / 86400
	Recent      []LoginEvent `json:"recent"`
}

// AdminSessions computes the cross-org session view; days outside [1,90]
// falls back to 30.
func (s *Store) AdminSessions(ctx context.Context, now int64, days int) (*AdminSessions, error) {
	if days <= 0 || days > 90 {
		days = 30
	}
	a := &AdminSessions{ByDay: []DayCount{}, Recent: []LoginEvent{}}

	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM login_events`).Scan(&a.Total); err != nil {
		return nil, err
	}
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM login_events WHERE at >= ?`, now-86400).Scan(&a.Logins24h); err != nil {
		return nil, err
	}
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM login_events WHERE at >= ?`, now-7*86400).Scan(&a.Logins7d); err != nil {
		return nil, err
	}
	if err := s.queryRow(ctx, `SELECT COUNT(DISTINCT owner_id) FROM login_events`).Scan(&a.UniqueUsers); err != nil {
		return nil, err
	}

	byDay, err := s.query(ctx,
		`SELECT at/86400 AS d, COUNT(*) AS c FROM login_events WHERE at >= ? GROUP BY at/86400 ORDER BY d`,
		now-int64(days)*86400)
	if err != nil {
		return nil, err
	}
	defer func() { _ = byDay.Close() }() //nolint:errcheck // best-effort cleanup
	for byDay.Next() {
		var dc DayCount
		if err := byDay.Scan(&dc.Day, &dc.Count); err != nil {
			return nil, err
		}
		a.ByDay = append(a.ByDay, dc)
	}
	if err := byDay.Err(); err != nil {
		return nil, err
	}

	a.Recent, err = collectRows(ctx, s,
		`SELECT owner_id, email, at FROM login_events ORDER BY at DESC, id DESC LIMIT 50`, nil,
		func(rows *sql.Rows, le *LoginEvent) error { return rows.Scan(&le.OwnerID, &le.Email, &le.At) })
	if err != nil {
		return nil, err
	}
	return a, nil
}

// AdminErrors returns recent crashed sandboxes fleet-wide; limit outside
// [1,500] falls back to 200.
func (s *Store) AdminErrors(ctx context.Context, limit int) ([]AdminErrorRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	return collectRows(ctx, s,
		`SELECT id, org_id, template, exit_state, destroyed_at FROM sandbox_history
		 WHERE `+crashedFilter+` ORDER BY destroyed_at DESC LIMIT ?`, []any{limit},
		func(rows *sql.Rows, e *AdminErrorRow) error {
			return rows.Scan(&e.ID, &e.OrgID, &e.Template, &e.ExitState, &e.DestroyedAt)
		})
}
