package store

import (
	"context"
	"database/sql"
	"errors"
)

// MarkNodeRemoved records that nodeID was revoked. Idempotent: removing an
// already-removed node (e.g. retried, or offline at the time) updates the
// record rather than erroring, and still means "refuse this id forever".
func (s *Store) MarkNodeRemoved(ctx context.Context, nodeID, zone string, removedAt int64) error {
	_, err := s.exec(ctx,
		`INSERT INTO removed_nodes (node_id, zone, removed_at) VALUES (?, ?, ?)
		 ON CONFLICT(node_id) DO UPDATE SET zone = ?, removed_at = ?`,
		nodeID, zone, removedAt, zone, removedAt,
	)
	return err
}

// NodeRemoved reports whether nodeID was revoked.
func (s *Store) NodeRemoved(ctx context.Context, nodeID string) (bool, error) {
	var exists int
	err := s.queryRow(ctx, `SELECT 1 FROM removed_nodes WHERE node_id = ?`, nodeID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// RemovedNodeIDs lists every revoked node id, so a join can keep them out of
// the names it allocates: a re-joined machine must get a fresh id rather than
// reusing (and un-revoking) one that was removed.
func (s *Store) RemovedNodeIDs(ctx context.Context) ([]string, error) {
	return collectRows(ctx, s, `SELECT node_id FROM removed_nodes ORDER BY node_id`, nil,
		func(rows *sql.Rows, id *string) error { return rows.Scan(id) })
}
