package store

import (
	"context"
	"time"
)

// AuditEntry is one recorded action. Detail is an optional JSON payload for
// operator-only surfaces; ListAudit never returns it.
type AuditEntry struct {
	OrgID  string `json:"org_id"`
	KeyID  string `json:"key_id"`
	Action string `json:"action"`
	Target string `json:"target"`
	At     int64  `json:"at"`
	Detail string `json:"detail,omitempty"`
}

// RecordAudit appends an audit row. Best-effort: callers log and never fail the request.
func (s *Store) RecordAudit(ctx context.Context, orgID, keyID, action, target string) error {
	return s.RecordAuditDetail(ctx, orgID, keyID, action, target, "")
}

// ListAudit returns the most recent audit entries for an org, newest first.
func (s *Store) ListAudit(ctx context.Context, orgID string, limit int) ([]AuditEntry, error) {
	if orgID == "" {
		orgID = "default"
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.query(ctx,
		`SELECT org_id, key_id, action, target, at FROM audit_log
		 WHERE org_id = ? ORDER BY at DESC, id DESC LIMIT ?`, orgID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.OrgID, &e.KeyID, &e.Action, &e.Target, &e.At); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RecordAuditDetail appends an audit row carrying a JSON detail payload.
func (s *Store) RecordAuditDetail(ctx context.Context, orgID, keyID, action, target, detail string) error {
	if orgID == "" {
		orgID = "default"
	}
	_, err := s.exec(ctx,
		`INSERT INTO audit_log (org_id, key_id, action, target, at, detail) VALUES (?, ?, ?, ?, ?, ?)`,
		orgID, keyID, action, target, time.Now().Unix(), detail,
	)
	return err
}

// ListAuditByTarget returns a target's audit rows oldest first, detail included,
// dropping the oldest when over limit. Cross-org: operator-only callers.
func (s *Store) ListAuditByTarget(ctx context.Context, target string, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	rows, err := s.query(ctx,
		`SELECT org_id, key_id, action, target, at, detail FROM audit_log
		 WHERE target = ? ORDER BY at DESC, id DESC LIMIT ?`, target, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.OrgID, &e.KeyID, &e.Action, &e.Target, &e.At, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Queried newest-first so LIMIT keeps the newest; reverse to oldest-first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}
