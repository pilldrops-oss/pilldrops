package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

var ErrConnectorNameTaken = errors.New("a connector with this name already exists")

type ConnectorTarget struct {
	Name string `json:"name"`
	Port int    `json:"port"`
	Host string `json:"host,omitempty"`
}

type Connector struct {
	ID, Name, OrgID, CreatedBy string
	CreatedAt, LastSeenAt      int64
	RevokedAt                  int64
	Targets                    []ConnectorTarget
	Subnets, Suffixes          []string
}

const connectorCols = `id, name, org_id, created_by, created_at, last_seen_at, revoked_at, targets, subnets, dns_suffixes`

func scanConnector(sc interface{ Scan(...any) error }) (Connector, error) {
	var c Connector
	var targets, subnets, suffixes string
	if err := sc.Scan(&c.ID, &c.Name, &c.OrgID, &c.CreatedBy, &c.CreatedAt, &c.LastSeenAt, &c.RevokedAt, &targets, &subnets, &suffixes); err != nil {
		return Connector{}, err
	}
	for _, f := range []struct {
		raw string
		dst any
	}{{targets, &c.Targets}, {subnets, &c.Subnets}, {suffixes, &c.Suffixes}} {
		if err := json.Unmarshal([]byte(f.raw), f.dst); err != nil {
			return Connector{}, err
		}
	}
	return c, nil
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "UNIQUE constraint failed") || strings.Contains(m, "duplicate key value")
}

func (s *Store) InsertConnector(ctx context.Context, c Connector) error {
	_, err := s.exec(ctx, `INSERT INTO connectors (`+connectorCols+`) VALUES (?, ?, ?, ?, ?, ?, 0, '[]', '[]', '[]')`,
		c.ID, c.Name, c.OrgID, c.CreatedBy, c.CreatedAt, c.LastSeenAt)
	if isUniqueViolation(err) {
		return ErrConnectorNameTaken
	}
	return err
}

func (s *Store) connectorWhere(ctx context.Context, where string, args ...any) (Connector, bool, error) {
	c, err := scanConnector(s.queryRow(ctx, `SELECT `+connectorCols+` FROM connectors WHERE `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return Connector{}, false, nil
	}
	return c, err == nil, err
}

func (s *Store) ConnectorByID(ctx context.Context, org, id string) (Connector, bool, error) {
	if org == "" {
		return s.connectorWhere(ctx, `id = ?`, id)
	}
	return s.connectorWhere(ctx, `id = ? AND org_id = ?`, id, org)
}

func (s *Store) ConnectorByName(ctx context.Context, org, name string) (Connector, bool, error) {
	return s.connectorWhere(ctx, `org_id = ? AND name = ? AND revoked_at = 0`, org, name)
}

func (s *Store) ListConnectors(ctx context.Context, org string) ([]Connector, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT `+connectorCols+` FROM connectors WHERE org_id = ? AND revoked_at = 0 ORDER BY name`), org)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }() //nolint:errcheck // read-only cursor
	var out []Connector
	for rows.Next() {
		c, err := scanConnector(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) CountConnectors(ctx context.Context, org string) (int, error) {
	var n int
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM connectors WHERE org_id = ? AND revoked_at = 0`, org).Scan(&n)
	return n, err
}

// SetConnectorAnnounced stores what the connector's instances announce now.
func (s *Store) SetConnectorAnnounced(ctx context.Context, id string, ts []ConnectorTarget, subnets, suffixes []string, now int64) error {
	if ts == nil {
		ts = []ConnectorTarget{}
	}
	if subnets == nil {
		subnets = []string{}
	}
	if suffixes == nil {
		suffixes = []string{}
	}
	tb, _ := json.Marshal(ts)
	sb, _ := json.Marshal(subnets)
	db, _ := json.Marshal(suffixes)
	_, err := s.exec(ctx, `UPDATE connectors SET targets = ?, subnets = ?, dns_suffixes = ?, last_seen_at = ? WHERE id = ?`, string(tb), string(sb), string(db), now, id)
	return err
}

func (s *Store) TouchConnector(ctx context.Context, id string, now int64) error {
	_, err := s.exec(ctx, `UPDATE connectors SET last_seen_at = ? WHERE id = ?`, now, id)
	return err
}

func (s *Store) RevokeConnector(ctx context.Context, org, id string, now int64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // no-op after commit
	res, err := tx.ExecContext(ctx, s.rebind(`UPDATE connectors SET revoked_at = ? WHERE id = ? AND org_id = ? AND revoked_at = 0`), now, id, org)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE connector_keys SET revoked_at = ? WHERE connector_id = ? AND revoked_at = 0`), now, id); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM sandbox_private_grants WHERE connector_id = ?`), id); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM private_grants WHERE connector_id = ?`), id); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

type ConnectorKey struct {
	ID, ConnectorID       string
	CreatedAt, LastUsedAt int64
	RevokedAt             int64
}

func (s *Store) CreateConnectorKey(ctx context.Context, connectorID string, now int64) (ConnectorKey, string, error) {
	plain := "pd_conn_" + randomToken(24)
	k := ConnectorKey{ID: "ck_" + randomToken(8), ConnectorID: connectorID, CreatedAt: now}
	_, err := s.exec(ctx, `INSERT INTO connector_keys (id, connector_id, key_hash, created_at) VALUES (?, ?, ?, ?)`,
		k.ID, k.ConnectorID, hashKey(plain), now)
	if err != nil {
		return ConnectorKey{}, "", err
	}
	return k, plain, nil
}

// ConnectorForKey resolves a live connector key to its connector and the key's id.
func (s *Store) ConnectorForKey(ctx context.Context, plaintext string, now int64) (Connector, string, bool, error) {
	if !strings.HasPrefix(plaintext, "pd_conn_") {
		return Connector{}, "", false, nil
	}
	var keyID, connID string
	err := s.queryRow(ctx, `SELECT id, connector_id FROM connector_keys WHERE key_hash = ? AND revoked_at = 0`, hashKey(plaintext)).Scan(&keyID, &connID)
	if errors.Is(err, sql.ErrNoRows) {
		return Connector{}, "", false, nil
	}
	if err != nil {
		return Connector{}, "", false, err
	}
	c, ok, err := s.ConnectorByID(ctx, "", connID)
	if err != nil || !ok || c.RevokedAt != 0 {
		return Connector{}, "", false, err
	}
	_, _ = s.exec(ctx, `UPDATE connector_keys SET last_used_at = ? WHERE id = ?`, now, keyID) //nolint:errcheck // best-effort timestamp
	return c, keyID, true, nil
}

// ConnectorKeyLive reports whether a connector's key exists and is not revoked.
func (s *Store) ConnectorKeyLive(ctx context.Context, connectorID, keyID string) (bool, error) {
	var n int
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM connector_keys WHERE id = ? AND connector_id = ? AND revoked_at = 0`, keyID, connectorID).Scan(&n)
	return n > 0, err
}

func (s *Store) ListConnectorKeys(ctx context.Context, connectorID string) ([]ConnectorKey, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT id, connector_id, created_at, last_used_at, revoked_at FROM connector_keys WHERE connector_id = ? AND revoked_at = 0 ORDER BY created_at`), connectorID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }() //nolint:errcheck // read-only cursor
	var out []ConnectorKey
	for rows.Next() {
		var k ConnectorKey
		if err := rows.Scan(&k.ID, &k.ConnectorID, &k.CreatedAt, &k.LastUsedAt, &k.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Store) RevokeConnectorKey(ctx context.Context, connectorID, keyID string, now int64) (bool, error) {
	res, err := s.exec(ctx, `UPDATE connector_keys SET revoked_at = ? WHERE id = ? AND connector_id = ? AND revoked_at = 0`, now, keyID, connectorID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
