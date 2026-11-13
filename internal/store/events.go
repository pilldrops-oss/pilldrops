package store

import (
	"context"
	"fmt"
)

// EventRecord is one sandbox lifecycle event plus the org's running-sandbox
// count immediately after it.
type EventRecord struct {
	ID          int64  `json:"-"`
	OrgID       string `json:"-"`
	SandboxID   string `json:"sandbox_id"`
	Event       string `json:"event"` // created | destroyed | paused | resumed
	Template    string `json:"template,omitempty"`
	ActiveAfter int    `json:"active_after"`
	At          int64  `json:"at"`
}

// RecordEvent appends a lifecycle event and queues it for the org's webhooks.
func (s *Store) RecordEvent(ctx context.Context, r EventRecord) error {
	if r.OrgID == "" {
		r.OrgID = "default"
	}
	err := s.queryRow(ctx,
		`INSERT INTO sandbox_events (org_id, sandbox_id, event, template, active_after, at)
		 VALUES (?, ?, ?, ?, ?, ?) RETURNING id`,
		r.OrgID, r.SandboxID, r.Event, r.Template, r.ActiveAfter, r.At,
	).Scan(&r.ID)
	if err != nil {
		return err
	}
	if err := s.enqueueSandboxWebhooks(ctx, r); err != nil {
		return fmt.Errorf("queue sandbox webhooks: %w", err)
	}
	return nil
}

// ListEvents returns an org's recent events, newest first.
func (s *Store) ListEvents(ctx context.Context, orgID string, limit int) ([]EventRecord, error) {
	if orgID == "" {
		orgID = "default"
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.query(ctx,
		`SELECT sandbox_id, event, template, active_after, at
		 FROM sandbox_events WHERE org_id = ? ORDER BY at DESC, id DESC LIMIT ?`,
		orgID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventRecord
	for rows.Next() {
		var r EventRecord
		if err := rows.Scan(&r.SandboxID, &r.Event, &r.Template, &r.ActiveAfter, &r.At); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// EventsAfter returns events with id > after, oldest first, across all orgs;
// it is the tail a notifier follows. Ordered by id so a cursor cannot skip
// same-second events.
func (s *Store) EventsAfter(ctx context.Context, after int64, limit int) ([]EventRecord, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.query(ctx,
		`SELECT id, org_id, sandbox_id, event, template, active_after, at
		 FROM sandbox_events WHERE id > ? ORDER BY id ASC LIMIT ?`, after, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventRecord
	for rows.Next() {
		var r EventRecord
		if err := rows.Scan(&r.ID, &r.OrgID, &r.SandboxID, &r.Event, &r.Template, &r.ActiveAfter, &r.At); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LatestEventID returns the highest event id, or 0 when there are none. A
// notifier starts here so a restart does not replay stale alerts.
func (s *Store) LatestEventID(ctx context.Context) (int64, error) {
	var id int64
	err := s.queryRow(ctx, `SELECT COALESCE(MAX(id),0) FROM sandbox_events`).Scan(&id)
	return id, err
}

// ConcurrencySeries returns the (at, active_after) points for an org since a
// time, oldest first, preceded by the last point before the window so a chart
// starts at the right level.
func (s *Store) ConcurrencySeries(ctx context.Context, orgID string, since int64) ([]EventRecord, error) {
	if orgID == "" {
		orgID = "default"
	}
	var out []EventRecord
	var anchor EventRecord
	err := s.queryRow(ctx,
		`SELECT at, active_after FROM sandbox_events
		 WHERE org_id = ? AND at < ? ORDER BY at DESC, id DESC LIMIT 1`,
		orgID, since,
	).Scan(&anchor.At, &anchor.ActiveAfter)
	if err == nil {
		anchor.At = since
		out = append(out, anchor)
	}
	rows, err := s.query(ctx,
		`SELECT at, active_after FROM sandbox_events
		 WHERE org_id = ? AND at >= ? ORDER BY at ASC, id ASC`,
		orgID, since,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r EventRecord
		if err := rows.Scan(&r.At, &r.ActiveAfter); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
