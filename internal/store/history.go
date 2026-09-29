package store

import (
	"context"
	"database/sql"
	"errors"
)

// HistoryRecord is a retained record of a destroyed sandbox, including its logs.
type HistoryRecord struct {
	ID    string `json:"id"`
	OrgID string `json:"org_id"`
	// Slug is the name the sandbox answered to while it ran, carried over so a
	// slug keeps working here as the help says it does everywhere an id does.
	// Empty for a sandbox that had none, and for one destroyed before this
	// column existed.
	Slug         string `json:"slug,omitempty"`
	Owner        string `json:"owner,omitempty"`
	Template     string `json:"template"`
	Image        string `json:"image,omitempty"`
	Node         string `json:"node,omitempty"`
	ExitState    string `json:"exit_state"`
	CreatedAt    int64  `json:"created_at"`
	DestroyedAt  int64  `json:"destroyed_at"`
	DurationSecs int64  `json:"duration_secs"`
	VCPU         int    `json:"vcpu"`
	MemMiB       int    `json:"mem_mib"`
	// Per-pod metered consumption over the sandbox's life.
	VCPUSeconds  float64 `json:"vcpu_seconds"`
	RAMGBSeconds float64 `json:"ram_gb_seconds"`
	// Source is "agent", "human" or "" (see ClassifySource); Client is the raw tag.
	Source string `json:"source,omitempty"`
	Client string `json:"client,omitempty"`
	Logs   string `json:"logs,omitempty"` // omitted from list; present on Get
}

// maxHistoryLogBytes bounds the retained log column; the tail is kept.
const maxHistoryLogBytes = 200 * 1024

// RecordHistory persists a destroyed sandbox's history. Idempotent on id.
func (s *Store) RecordHistory(ctx context.Context, r HistoryRecord) error {
	if r.OrgID == "" {
		r.OrgID = "default"
	}
	if len(r.Logs) > maxHistoryLogBytes {
		r.Logs = r.Logs[len(r.Logs)-maxHistoryLogBytes:]
	}
	// source/client fall back to the sandbox row, which still exists because
	// RecordHistory runs before DeleteSandbox.
	_, err := s.exec(ctx,
		`INSERT INTO sandbox_history
		   (id, org_id, owner, template, image, node, exit_state, created_at, destroyed_at, duration_secs, vcpu, mem_mib, vcpu_seconds, ram_gb_seconds, logs, source, client, slug)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		   COALESCE(NULLIF(?, ''), (SELECT source FROM sandboxes WHERE id = ?), ''),
		   COALESCE(NULLIF(?, ''), (SELECT client FROM sandboxes WHERE id = ?), ''),
		   COALESCE((SELECT slug FROM sandboxes WHERE id = ?), ''))
		 ON CONFLICT(id) DO NOTHING`,
		r.ID, r.OrgID, r.Owner, r.Template, r.Image, r.Node, r.ExitState,
		r.CreatedAt, r.DestroyedAt, r.DurationSecs, r.VCPU, r.MemMiB,
		r.VCPUSeconds, r.RAMGBSeconds, r.Logs, r.Source, r.ID, r.Client, r.ID, r.ID,
	)
	return err
}

// ListHistory returns an org's destroyed sandboxes, newest first, without logs.
func (s *Store) ListHistory(ctx context.Context, orgID string, limit int) ([]HistoryRecord, error) {
	return s.ListHistoryPage(ctx, orgID, limit, 0)
}

// ListHistoryPage is ListHistory with limit/offset paging.
func (s *Store) ListHistoryPage(ctx context.Context, orgID string, limit, offset int) ([]HistoryRecord, error) {
	if orgID == "" {
		orgID = "default"
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.query(ctx,
		`SELECT id, org_id, owner, template, image, node, exit_state, created_at, destroyed_at, duration_secs, vcpu, mem_mib, vcpu_seconds, ram_gb_seconds, source, client, slug
		 FROM sandbox_history WHERE org_id = ? ORDER BY destroyed_at DESC LIMIT ? OFFSET ?`,
		orgID, limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HistoryRecord
	for rows.Next() {
		var r HistoryRecord
		if err := rows.Scan(&r.ID, &r.OrgID, &r.Owner, &r.Template, &r.Image, &r.Node,
			&r.ExitState, &r.CreatedAt, &r.DestroyedAt, &r.DurationSecs, &r.VCPU, &r.MemMiB,
			&r.VCPUSeconds, &r.RAMGBSeconds, &r.Source, &r.Client, &r.Slug); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetHistory returns one retained record including its logs, by sandbox id or
// by the slug it answered to.
//
// The slug has to be matched here rather than by the id-resolving middleware
// above: that resolver reads the LIVE sandboxes table, and a history record
// exists precisely because the sandbox is gone. A slug can be reused after a
// destroy, so the newest record wins — the same one `sandbox get <slug>` would
// have answered for a moment ago.
func (s *Store) GetHistory(ctx context.Context, id string) (HistoryRecord, bool, error) {
	var r HistoryRecord
	err := s.queryRow(ctx,
		`SELECT id, org_id, owner, template, image, node, exit_state, created_at, destroyed_at, duration_secs, vcpu, mem_mib, vcpu_seconds, ram_gb_seconds, source, client, logs, slug
		 FROM sandbox_history WHERE id = ? OR (slug <> '' AND slug = ?)
		 ORDER BY destroyed_at DESC LIMIT 1`, id, id,
	).Scan(&r.ID, &r.OrgID, &r.Owner, &r.Template, &r.Image, &r.Node,
		&r.ExitState, &r.CreatedAt, &r.DestroyedAt, &r.DurationSecs, &r.VCPU, &r.MemMiB,
		&r.VCPUSeconds, &r.RAMGBSeconds, &r.Source, &r.Client, &r.Logs, &r.Slug)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	return r, true, nil
}

// Span is a sandbox's lifetime window, for deriving concurrency series.
type Span struct {
	CreatedAt   int64
	DestroyedAt int64
}

// HistorySpans returns lifetime spans of an org's destroyed sandboxes with
// destroyed_at >= since.
func (s *Store) HistorySpans(ctx context.Context, orgID string, since int64) ([]Span, error) {
	if orgID == "" {
		orgID = "default"
	}
	rows, err := s.query(ctx,
		`SELECT created_at, destroyed_at FROM sandbox_history
		 WHERE org_id = ? AND destroyed_at >= ?`, orgID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Span
	for rows.Next() {
		var sp Span
		if err := rows.Scan(&sp.CreatedAt, &sp.DestroyedAt); err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}
