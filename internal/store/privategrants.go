package store

import (
	"context"
	"errors"
	"strings"
)

var ErrGrantExists = errors.New("this grant already exists")

const (
	GrantSubjectSandbox = "sandbox"
	GrantSubjectTag     = "tag"

	GrantKindTarget = "target"
	GrantKindSubnet = "subnet"
	GrantKindDNS    = "dns"
)

// Grant lets one sandbox, or every sandbox in a network tag, reach a target,
// a subnet or the names under a DNS suffix behind a connector.
type Grant struct {
	ID, OrgID, ConnectorID, ConnectorName string
	SubjectKind, Subject, Kind, Value     string
	CreatedAt                             int64
}

const grantRowsSelect = `SELECT g.id, g.org_id, g.connector_id, c.name, g.subject_kind, g.subject, g.kind, g.value, g.created_at
	FROM private_grants g JOIN connectors c ON c.id = g.connector_id AND c.revoked_at = 0 WHERE `

// grantsWhere runs grantRowsSelect; where is a constant, never caller data.
func (s *Store) grantsWhere(ctx context.Context, where string, args ...any) ([]Grant, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(grantRowsSelect+where+` ORDER BY g.kind, g.value, g.id`), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }() //nolint:errcheck // read-only cursor
	var out []Grant
	for rows.Next() {
		var g Grant
		if err := rows.Scan(&g.ID, &g.OrgID, &g.ConnectorID, &g.ConnectorName, &g.SubjectKind, &g.Subject, &g.Kind, &g.Value, &g.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Store) oneGrant(ctx context.Context, where string, args ...any) (Grant, bool, error) {
	gs, err := s.grantsWhere(ctx, where, args...)
	if err != nil || len(gs) == 0 {
		return Grant{}, false, err
	}
	return gs[0], true, nil
}

func mirrorsOldTable(g Grant) bool {
	return g.SubjectKind == GrantSubjectSandbox && g.Kind == GrantKindTarget
}

// AddGrant stores g under a new id. A sandbox's target grant is written to
// sandbox_private_grants too, so a control plane rolled back to phase 1 keeps it.
func (s *Store) AddGrant(ctx context.Context, g Grant) (Grant, error) {
	g.ID = "pg_" + randomToken(12)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Grant{}, err
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // no-op after commit
	_, err = tx.ExecContext(ctx, s.rebind(`INSERT INTO private_grants (id, org_id, connector_id, subject_kind, subject, kind, value, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		g.ID, g.OrgID, g.ConnectorID, g.SubjectKind, g.Subject, g.Kind, g.Value, g.CreatedAt)
	if isUniqueViolation(err) {
		return Grant{}, ErrGrantExists
	}
	if err != nil {
		return Grant{}, err
	}
	if mirrorsOldTable(g) {
		if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO sandbox_private_grants (sandbox_id, connector_id, target, created_at) VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`),
			g.Subject, g.ConnectorID, g.Value, g.CreatedAt); err != nil {
			return Grant{}, err
		}
	}
	return g, tx.Commit()
}

// RemoveGrant deletes g, and its sandbox_private_grants row when it has one.
func (s *Store) RemoveGrant(ctx context.Context, g Grant) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // no-op after commit
	if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM private_grants WHERE id = ?`), g.ID); err != nil {
		return err
	}
	if mirrorsOldTable(g) {
		if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM sandbox_private_grants WHERE sandbox_id = ? AND connector_id = ? AND target = ?`),
			g.Subject, g.ConnectorID, g.Value); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) GrantByID(ctx context.Context, org, id string) (Grant, bool, error) {
	return s.oneGrant(ctx, `g.org_id = ? AND g.id = ?`, org, id)
}

func (s *Store) FindGrant(ctx context.Context, subjectKind, subject, connectorID, kind, value string) (Grant, bool, error) {
	return s.oneGrant(ctx, `g.subject_kind = ? AND g.subject = ? AND g.connector_id = ? AND g.kind = ? AND g.value = ?`,
		subjectKind, subject, connectorID, kind, value)
}

// SandboxGrants lists a sandbox's own grants, not its tags'.
func (s *Store) SandboxGrants(ctx context.Context, sandboxID string) ([]Grant, error) {
	return s.grantsWhere(ctx, `g.subject_kind = 'sandbox' AND g.subject = ?`, sandboxID)
}

// EffectiveGrants is a sandbox's own grants plus those of every tag it is in.
func (s *Store) EffectiveGrants(ctx context.Context, org, sandboxID string, tags []string) ([]Grant, error) {
	where := `g.org_id = ? AND ((g.subject_kind = 'sandbox' AND g.subject = ?)`
	args := []any{org, sandboxID}
	if len(tags) > 0 {
		where += ` OR (g.subject_kind = 'tag' AND g.subject IN (?` + strings.Repeat(", ?", len(tags)-1) + `))`
		for _, t := range tags {
			args = append(args, t)
		}
	}
	return s.grantsWhere(ctx, where+`)`, args...)
}

// TagGrants lists org's tag grants, for one tag or, with tag "", all of them.
func (s *Store) TagGrants(ctx context.Context, org, tag string) ([]Grant, error) {
	if tag == "" {
		return s.grantsWhere(ctx, `g.org_id = ? AND g.subject_kind = 'tag'`, org)
	}
	return s.grantsWhere(ctx, `g.org_id = ? AND g.subject_kind = 'tag' AND g.subject = ?`, org, tag)
}

func (s *Store) CountTagGrants(ctx context.Context, org string) (int, error) {
	var n int
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM private_grants WHERE org_id = ? AND subject_kind = 'tag'`, org).Scan(&n)
	return n, err
}

func (s *Store) GrantsOnConnector(ctx context.Context, connectorID string) ([]Grant, error) {
	return s.grantsWhere(ctx, `g.connector_id = ?`, connectorID)
}

// DeletePrivateGrantsForSandbox drops a sandbox's own grants from both tables.
func (s *Store) DeletePrivateGrantsForSandbox(ctx context.Context, sandboxID string) error {
	if _, err := s.exec(ctx, `DELETE FROM private_grants WHERE subject_kind = 'sandbox' AND subject = ?`, sandboxID); err != nil {
		return err
	}
	_, err := s.exec(ctx, `DELETE FROM sandbox_private_grants WHERE sandbox_id = ?`, sandboxID)
	return err
}

type PrivateConn struct {
	ID, OrgID, SandboxID, ConnectorID, Target, Dest, NodeID, Result string
	StartedAt, EndedAt, BytesIn, BytesOut                           int64
}

func (s *Store) InsertPrivateConn(ctx context.Context, c PrivateConn) error {
	_, err := s.exec(ctx, `INSERT INTO private_connections (id, org_id, sandbox_id, connector_id, target, dest, node_id, started_at, ended_at, bytes_in, bytes_out, result)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.OrgID, c.SandboxID, c.ConnectorID, c.Target, c.Dest, c.NodeID, c.StartedAt, c.EndedAt, c.BytesIn, c.BytesOut, c.Result)
	return err
}

func (s *Store) PrivateConns(ctx context.Context, connectorID string, limit int) ([]PrivateConn, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT id, org_id, sandbox_id, connector_id, target, dest, node_id, started_at, ended_at, bytes_in, bytes_out, result
		FROM private_connections WHERE connector_id = ? ORDER BY started_at DESC, id DESC LIMIT ?`), connectorID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }() //nolint:errcheck // read-only cursor
	var out []PrivateConn
	for rows.Next() {
		var c PrivateConn
		if err := rows.Scan(&c.ID, &c.OrgID, &c.SandboxID, &c.ConnectorID, &c.Target, &c.Dest, &c.NodeID, &c.StartedAt, &c.EndedAt, &c.BytesIn, &c.BytesOut, &c.Result); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
