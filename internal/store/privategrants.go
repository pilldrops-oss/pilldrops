package store

import (
	"context"
	"database/sql"
	"errors"
)

var ErrPrivateNameTaken = errors.New("this sandbox already has a private target with that name")

type PrivateGrant struct {
	SandboxID, ConnectorID, ConnectorName, Target string
	CreatedAt                                     int64
}

func (s *Store) AddPrivateGrant(ctx context.Context, g PrivateGrant) error {
	_, err := s.exec(ctx, `INSERT INTO sandbox_private_grants (sandbox_id, connector_id, target, created_at) VALUES (?, ?, ?, ?)`,
		g.SandboxID, g.ConnectorID, g.Target, g.CreatedAt)
	if isUniqueViolation(err) {
		return ErrPrivateNameTaken
	}
	return err
}

func (s *Store) RemovePrivateGrant(ctx context.Context, sandboxID, connectorID, target string) (bool, error) {
	res, err := s.exec(ctx, `DELETE FROM sandbox_private_grants WHERE sandbox_id = ? AND connector_id = ? AND target = ?`, sandboxID, connectorID, target)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

const grantSelect = `SELECT g.sandbox_id, g.connector_id, c.name, g.target, g.created_at
	FROM sandbox_private_grants g JOIN connectors c ON c.id = g.connector_id AND c.revoked_at = 0`

func (s *Store) PrivateGrants(ctx context.Context, sandboxID string) ([]PrivateGrant, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(grantSelect+` WHERE g.sandbox_id = ? ORDER BY g.target`), sandboxID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }() //nolint:errcheck // read-only cursor
	var out []PrivateGrant
	for rows.Next() {
		var g PrivateGrant
		if err := rows.Scan(&g.SandboxID, &g.ConnectorID, &g.ConnectorName, &g.Target, &g.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Store) PrivateGrantFor(ctx context.Context, sandboxID, target string) (PrivateGrant, bool, error) {
	var g PrivateGrant
	err := s.queryRow(ctx, grantSelect+` WHERE g.sandbox_id = ? AND g.target = ?`, sandboxID, target).
		Scan(&g.SandboxID, &g.ConnectorID, &g.ConnectorName, &g.Target, &g.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PrivateGrant{}, false, nil
	}
	return g, err == nil, err
}

// SandboxesGrantedConnector lists the sandboxes holding any grant on a connector.
func (s *Store) SandboxesGrantedConnector(ctx context.Context, connectorID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT DISTINCT sandbox_id FROM sandbox_private_grants WHERE connector_id = ?`), connectorID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }() //nolint:errcheck // read-only cursor
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) DeletePrivateGrantsForSandbox(ctx context.Context, sandboxID string) error {
	_, err := s.exec(ctx, `DELETE FROM sandbox_private_grants WHERE sandbox_id = ?`, sandboxID)
	return err
}

type PrivateConn struct {
	ID, OrgID, SandboxID, ConnectorID, Target, NodeID, Result string
	StartedAt, EndedAt, BytesIn, BytesOut                     int64
}

func (s *Store) InsertPrivateConn(ctx context.Context, c PrivateConn) error {
	_, err := s.exec(ctx, `INSERT INTO private_connections (id, org_id, sandbox_id, connector_id, target, node_id, started_at, ended_at, bytes_in, bytes_out, result)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.OrgID, c.SandboxID, c.ConnectorID, c.Target, c.NodeID, c.StartedAt, c.EndedAt, c.BytesIn, c.BytesOut, c.Result)
	return err
}

func (s *Store) PrivateConns(ctx context.Context, connectorID string, limit int) ([]PrivateConn, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT id, org_id, sandbox_id, connector_id, target, node_id, started_at, ended_at, bytes_in, bytes_out, result
		FROM private_connections WHERE connector_id = ? ORDER BY started_at DESC, id DESC LIMIT ?`), connectorID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }() //nolint:errcheck // read-only cursor
	var out []PrivateConn
	for rows.Next() {
		var c PrivateConn
		if err := rows.Scan(&c.ID, &c.OrgID, &c.SandboxID, &c.ConnectorID, &c.Target, &c.NodeID, &c.StartedAt, &c.EndedAt, &c.BytesIn, &c.BytesOut, &c.Result); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
