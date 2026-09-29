package store

import (
	"context"
	"database/sql"
)

// maxThreadBytes caps a stored transcript so one runaway client cannot fill the table.
const maxThreadBytes = 256 * 1024

// Thread is one conversation. Turns is an opaque client-owned transcript blob.
type Thread struct {
	ID    string `json:"id"`
	OrgID string `json:"org_id"`
	Owner string `json:"owner,omitempty"`
	Title string `json:"title"`
	// Turns is omitted from ListThreads.
	Turns     string `json:"turns,omitempty"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// SaveThread inserts or updates. Idempotent on id.
func (s *Store) SaveThread(ctx context.Context, t Thread) error {
	if t.OrgID == "" {
		t.OrgID = "default"
	}
	// Keep the tail; the end of a conversation is the part still in play.
	if len(t.Turns) > maxThreadBytes {
		t.Turns = t.Turns[len(t.Turns)-maxThreadBytes:]
	}
	_, err := s.exec(ctx, `
		INSERT INTO threads (id, org_id, owner, title, turns, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			title = EXCLUDED.title,
			turns = EXCLUDED.turns,
			updated_at = EXCLUDED.updated_at`,
		t.ID, t.OrgID, t.Owner, t.Title, t.Turns, t.CreatedAt, t.UpdatedAt)
	return err
}

// GetThread returns one thread with its transcript, or nil.
func (s *Store) GetThread(ctx context.Context, orgID, id string) (*Thread, error) {
	if orgID == "" {
		orgID = "default"
	}
	row := s.queryRow(ctx, `
		SELECT id, org_id, owner, title, turns, created_at, updated_at
		FROM threads WHERE org_id = ? AND id = ?`, orgID, id)
	var t Thread
	if err := row.Scan(&t.ID, &t.OrgID, &t.Owner, &t.Title, &t.Turns, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, nil
	}
	return &t, nil
}

// ListThreads returns an org's conversations, most recently updated first, without transcripts.
func (s *Store) ListThreads(ctx context.Context, orgID string, limit int) ([]Thread, error) {
	if orgID == "" {
		orgID = "default"
	}
	if limit <= 0 {
		limit = 50
	}
	return collectRows(ctx, s, `
		SELECT id, org_id, owner, title, created_at, updated_at
		FROM threads
		WHERE org_id = ?
		ORDER BY updated_at DESC, created_at DESC
		LIMIT ?`, []any{orgID, limit},
		func(rows *sql.Rows, t *Thread) error {
			return rows.Scan(&t.ID, &t.OrgID, &t.Owner, &t.Title, &t.CreatedAt, &t.UpdatedAt)
		})
}

// DeleteThread removes a thread. Org-scoped.
func (s *Store) DeleteThread(ctx context.Context, orgID, id string) error {
	if orgID == "" {
		orgID = "default"
	}
	_, err := s.exec(ctx, `DELETE FROM threads WHERE org_id = ? AND id = ?`, orgID, id)
	return err
}
