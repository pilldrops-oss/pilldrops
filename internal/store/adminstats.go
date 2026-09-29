package store

import (
	"context"
	"database/sql"
)

// Fleet-wide analytics for the operator dashboard. Cross-org, so the API tier
// must gate it to operators. Day buckets use unix/86400 to stay portable across
// SQLite and Postgres.

// NameCount is a labeled count for breakdown charts (templates, exit states).
type NameCount struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
}

// DayCount is a per-day bucket; Day is the UTC day index (unix / 86400).
type DayCount struct {
	Day   int64 `json:"day"`
	Count int64 `json:"count"`
}

// AdminErrorRow is a compact crashed/failed sandbox for the overview error feed.
type AdminErrorRow struct {
	ID          string `json:"id"`
	OrgID       string `json:"org_id"`
	Template    string `json:"template"`
	ExitState   string `json:"exit_state"`
	DestroyedAt int64  `json:"destroyed_at"`
}

// AdminEventRow is one fleet-wide lifecycle event, with org_id exposed.
type AdminEventRow struct {
	OrgID     string `json:"org_id"`
	SandboxID string `json:"sandbox_id"`
	Event     string `json:"event"`
	Template  string `json:"template"`
	At        int64  `json:"at"`
}

// AdminOverview is the fleet-wide analytics snapshot.
type AdminOverview struct {
	TotalUsers      int64           `json:"total_users"`
	ActiveUsers24h  int64           `json:"active_users_24h"`
	Logins          int64           `json:"logins"`
	ActiveSandboxes int64           `json:"active_sandboxes"`
	TotalCreated    int64           `json:"total_created"`
	TotalFunctions  int64           `json:"total_functions"`
	CrashedCount    int64           `json:"crashed_count"`
	Templates       []NameCount     `json:"templates"`
	ExitStates      []NameCount     `json:"exit_states"`
	BySource        []NameCount     `json:"by_source"` // created by agent vs human
	CreatedByDay    []DayCount      `json:"created_by_day"`
	RecentErrors    []AdminErrorRow `json:"recent_errors"`
	RecentActivity  []AdminEventRow `json:"recent_activity"`
}

// crashedFilter is the SQL predicate for exit states that count as failures.
const crashedFilter = `exit_state IN ('crashed','failed','error','oom','oomkilled')`

// AdminOverview computes the fleet-wide analytics snapshot; days is the
// created-per-day window (default 14, max 90).
func (s *Store) AdminOverview(ctx context.Context, now int64, days int) (*AdminOverview, error) {
	if days <= 0 || days > 90 {
		days = 14
	}
	o := &AdminOverview{
		Templates:      []NameCount{},
		ExitStates:     []NameCount{},
		BySource:       []NameCount{},
		CreatedByDay:   []DayCount{},
		RecentErrors:   []AdminErrorRow{},
		RecentActivity: []AdminEventRow{},
	}

	scalar := func(dst *int64, q string, args ...any) error {
		return s.queryRow(ctx, q, args...).Scan(dst)
	}

	if err := scalar(&o.TotalUsers, `SELECT COUNT(*) FROM users`); err != nil {
		return nil, err
	}
	if err := scalar(&o.ActiveUsers24h, `SELECT COUNT(*) FROM users WHERE last_seen >= ?`, now-86400); err != nil {
		return nil, err
	}
	if err := s.queryRow(ctx, `SELECT COALESCE(SUM(login_count),0) FROM users`).Scan(&o.Logins); err != nil {
		return nil, err
	}
	if err := scalar(&o.ActiveSandboxes, `SELECT COUNT(*) FROM sandboxes`); err != nil {
		return nil, err
	}
	var destroyed int64
	if err := scalar(&destroyed, `SELECT COUNT(*) FROM sandbox_history`); err != nil {
		return nil, err
	}
	o.TotalCreated = o.ActiveSandboxes + destroyed
	if err := scalar(&o.TotalFunctions, `SELECT COUNT(*) FROM functions`); err != nil {
		return nil, err
	}
	if err := scalar(&o.CrashedCount, `SELECT COUNT(*) FROM sandbox_history WHERE `+crashedFilter); err != nil {
		return nil, err
	}

	if err := s.scanNameCounts(ctx, &o.Templates,
		`SELECT template, COUNT(*) AS c FROM (
		   SELECT template FROM sandboxes
		   UNION ALL
		   SELECT template FROM sandbox_history
		 ) t WHERE template <> '' GROUP BY template ORDER BY c DESC LIMIT 8`); err != nil {
		return nil, err
	}

	if err := s.scanNameCounts(ctx, &o.ExitStates,
		`SELECT exit_state, COUNT(*) AS c FROM sandbox_history GROUP BY exit_state ORDER BY c DESC LIMIT 12`); err != nil {
		return nil, err
	}
	for i := range o.ExitStates {
		if o.ExitStates[i].Name == "" {
			o.ExitStates[i].Name = "unknown"
		}
	}

	if err := s.scanNameCounts(ctx, &o.BySource,
		`SELECT source, COUNT(*) AS c FROM (
		   SELECT source FROM sandboxes
		   UNION ALL
		   SELECT source FROM sandbox_history
		 ) t GROUP BY source ORDER BY c DESC`); err != nil {
		return nil, err
	}
	for i := range o.BySource {
		if o.BySource[i].Name == "" {
			o.BySource[i].Name = "unknown"
		}
	}

	var err error
	o.CreatedByDay, err = collectRows(ctx, s,
		`SELECT created_at/86400 AS d, COUNT(*) AS c FROM sandbox_history
		 WHERE created_at >= ? GROUP BY created_at/86400 ORDER BY d`,
		[]any{now - int64(days)*86400},
		func(rows *sql.Rows, dc *DayCount) error { return rows.Scan(&dc.Day, &dc.Count) })
	if err != nil {
		return nil, err
	}

	o.RecentErrors, err = collectRows(ctx, s,
		`SELECT id, org_id, template, exit_state, destroyed_at FROM sandbox_history
		 WHERE `+crashedFilter+` ORDER BY destroyed_at DESC LIMIT 20`, nil,
		func(rows *sql.Rows, e *AdminErrorRow) error {
			return rows.Scan(&e.ID, &e.OrgID, &e.Template, &e.ExitState, &e.DestroyedAt)
		})
	if err != nil {
		return nil, err
	}

	o.RecentActivity, err = collectRows(ctx, s,
		`SELECT org_id, sandbox_id, event, template, at FROM sandbox_events
		 ORDER BY at DESC, id DESC LIMIT 25`, nil,
		func(rows *sql.Rows, a *AdminEventRow) error {
			return rows.Scan(&a.OrgID, &a.SandboxID, &a.Event, &a.Template, &a.At)
		})
	if err != nil {
		return nil, err
	}
	return o, nil
}

// scanNameCounts runs a (name, count) aggregate query, appending to dst.
func (s *Store) scanNameCounts(ctx context.Context, dst *[]NameCount, q string, args ...any) error {
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var nc NameCount
		if err := rows.Scan(&nc.Name, &nc.Count); err != nil {
			return err
		}
		*dst = append(*dst, nc)
	}
	return rows.Err()
}
