package store

import (
	"context"
)

// The activation funnel: signup, first workstation, still alive a day later.
// Derived from history rather than emitted as events, since nothing happens at
// the 24-hour mark to emit.

// FunnelSummary is the activation funnel over a window.
type FunnelSummary struct {
	// Signups is distinct accounts whose first login lands in the window.
	Signups int `json:"signups"`
	// Created is those signups that went on to create at least one sandbox.
	Created int `json:"created"`
	// AliveAt24h is those that had a sandbox still alive 24 hours after their first one.
	AliveAt24h int `json:"alive_at_24h"`
}

// day is the survival window, in seconds.
const day = int64(24 * 60 * 60)

// Funnel computes the activation funnel for accounts whose first login is at
// or after since.
func (s *Store) Funnel(ctx context.Context, since int64) (FunnelSummary, error) {
	var f FunnelSummary

	if err := s.queryRow(ctx,
		`SELECT COUNT(*) FROM users WHERE first_seen >= ?`, since,
	).Scan(&f.Signups); err != nil {
		return f, err
	}

	// For customer orgs the org id is the owner id.
	if err := s.queryRow(ctx,
		`SELECT COUNT(DISTINCT u.owner_id)
		   FROM users u
		  WHERE u.first_seen >= ?
		    AND EXISTS (SELECT 1 FROM sandbox_events e
		                 WHERE e.org_id = u.owner_id AND e.event = 'created')`, since,
	).Scan(&f.Created); err != nil {
		return f, err
	}

	// Survival is measured from the owner's first create, not from signup. A
	// sandbox is alive at the mark when it began before it and either outlived
	// it (history) or is still running (sandboxes).
	if err := s.queryRow(ctx,
		`SELECT COUNT(*) FROM (
		   SELECT u.owner_id,
		          (SELECT MIN(e.at) FROM sandbox_events e
		            WHERE e.org_id = u.owner_id AND e.event = 'created') AS first_at
		     FROM users u
		    WHERE u.first_seen >= ?
		 ) t
		  WHERE t.first_at IS NOT NULL
		    AND (
		      EXISTS (SELECT 1 FROM sandbox_history h
		               WHERE h.org_id = t.owner_id
		                 AND h.created_at <= t.first_at + ?
		                 AND h.destroyed_at >= t.first_at + ?)
		      OR EXISTS (SELECT 1 FROM sandboxes sb
		                  WHERE sb.org_id = t.owner_id
		                    AND sb.created_at <= t.first_at + ?)
		    )`, since, day, day, day,
	).Scan(&f.AliveAt24h); err != nil {
		return f, err
	}
	return f, nil
}

// SignupsBySource counts waitlist signups by the landing page's source tag.
func (s *Store) SignupsBySource(ctx context.Context, since int64) (map[string]int, error) {
	rows, err := s.query(ctx,
		`SELECT COALESCE(NULLIF(source, ''), 'unknown') AS src, COUNT(*)
		   FROM waitlist WHERE created_at >= ? GROUP BY src ORDER BY COUNT(*) DESC`, since,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var src string
		var n int
		if err := rows.Scan(&src, &n); err != nil {
			return nil, err
		}
		out[src] = n
	}
	return out, rows.Err()
}
