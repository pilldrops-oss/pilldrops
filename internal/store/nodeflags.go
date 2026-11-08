package store

import (
	"context"
	"database/sql"
)

// NodeFlagsRow is one node's cordon and drain flags.
type NodeFlagsRow struct {
	NodeID             string
	Cordoned, Draining bool
}

// SaveNodeFlags records a node's flags. A node with neither has no row.
func (s *Store) SaveNodeFlags(ctx context.Context, row NodeFlagsRow) error {
	if !row.Cordoned && !row.Draining {
		_, err := s.exec(ctx, `DELETE FROM node_flags WHERE node_id = ?`, row.NodeID)
		return err
	}
	_, err := s.exec(ctx,
		`INSERT INTO node_flags (node_id, cordoned, draining) VALUES (?, ?, ?)
		 ON CONFLICT(node_id) DO UPDATE SET cordoned = ?, draining = ?`,
		row.NodeID, row.Cordoned, row.Draining, row.Cordoned, row.Draining,
	)
	return err
}

// NodeFlags lists every node with a flag set.
func (s *Store) NodeFlags(ctx context.Context) ([]NodeFlagsRow, error) {
	return collectRows(ctx, s, `SELECT node_id, cordoned, draining FROM node_flags ORDER BY node_id`, nil,
		func(rows *sql.Rows, row *NodeFlagsRow) error {
			return rows.Scan(&row.NodeID, &row.Cordoned, &row.Draining)
		})
}
