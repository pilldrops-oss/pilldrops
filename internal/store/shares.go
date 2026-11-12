package store

import (
	"context"
	"database/sql"
	"errors"
)

// A share is a public link to an app on a developer's own machine. The row is
// what lets a link survive an ingress restart and a takedown stick. Times are
// unix seconds; 0 means not yet.

// Share is one row of shares.
type Share struct {
	ID, Name, OrgID, CreatedBy string
	CreatedAt, ExpiresAt       int64
	LastSeenAt                 int64
	ClosedAt, BlockedAt        int64
	BytesIn, BytesOut          int64
}

// State names why a share does or does not serve. Blocked outranks closed so a
// taken-down link keeps saying so.
func (s Share) State(now int64) string {
	switch {
	case s.BlockedAt != 0:
		return "blocked"
	case s.ClosedAt != 0:
		return "closed"
	case now >= s.ExpiresAt:
		return "expired"
	}
	return "open"
}

const shareCols = `id, name, org_id, created_by, created_at, expires_at, last_seen_at, closed_at, blocked_at, bytes_in, bytes_out`

func scanShare(row *sql.Row) (Share, bool, error) {
	var s Share
	err := row.Scan(&s.ID, &s.Name, &s.OrgID, &s.CreatedBy, &s.CreatedAt, &s.ExpiresAt,
		&s.LastSeenAt, &s.ClosedAt, &s.BlockedAt, &s.BytesIn, &s.BytesOut)
	if errors.Is(err, sql.ErrNoRows) {
		return Share{}, false, nil
	}
	return s, err == nil, err
}

// InsertShare fails on a taken name, which the caller retries with a new one.
func (s *Store) InsertShare(ctx context.Context, sh Share) error {
	_, err := s.exec(ctx,
		`INSERT INTO shares (`+shareCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sh.ID, sh.Name, sh.OrgID, sh.CreatedBy, sh.CreatedAt, sh.ExpiresAt,
		sh.LastSeenAt, sh.ClosedAt, sh.BlockedAt, sh.BytesIn, sh.BytesOut)
	return err
}

// ShareByID returns one row whatever its org; the caller checks ownership.
func (s *Store) ShareByID(ctx context.Context, id string) (Share, bool, error) {
	return scanShare(s.queryRow(ctx, `SELECT `+shareCols+` FROM shares WHERE id = ?`, id))
}

// ShareByName returns the row behind a public host.
func (s *Store) ShareByName(ctx context.Context, name string) (Share, bool, error) {
	return scanShare(s.queryRow(ctx, `SELECT `+shareCols+` FROM shares WHERE name = ?`, name))
}

// CountActiveShares counts an org's shares against its limit. A share unseen
// since staleBefore does not count, so a crashed CLI does not hold a slot until
// its link expires.
func (s *Store) CountActiveShares(ctx context.Context, org string, now, staleBefore int64) (int, error) {
	var n int
	err := s.queryRow(ctx,
		`SELECT COUNT(*) FROM shares WHERE org_id = ? AND closed_at = 0 AND blocked_at = 0
		 AND expires_at > ? AND (last_seen_at > ? OR created_at > ?)`,
		org, now, staleBefore, staleBefore).Scan(&n)
	return n, err
}

// CountConnectedShares counts an org's open shares seen since staleBefore,
// other than except, so a reconnecting share does not count against itself.
func (s *Store) CountConnectedShares(ctx context.Context, org, except string, now, staleBefore int64) (int, error) {
	var n int
	err := s.queryRow(ctx,
		`SELECT COUNT(*) FROM shares WHERE org_id = ? AND id <> ? AND closed_at = 0 AND blocked_at = 0
		 AND expires_at > ? AND last_seen_at > ?`,
		org, except, now, staleBefore).Scan(&n)
	return n, err
}

// TouchShare records that the tunnel is connected.
func (s *Store) TouchShare(ctx context.Context, id string, now int64) error {
	_, err := s.exec(ctx, `UPDATE shares SET last_seen_at = ? WHERE id = ?`, now, id)
	return err
}

// CloseShare ends a share. Org-scoped in the statement: another org's id
// answers false, which the caller reports as a 404.
func (s *Store) CloseShare(ctx context.Context, org, id string, now int64) (bool, error) {
	res, err := s.exec(ctx, `UPDATE shares SET closed_at = ? WHERE id = ? AND org_id = ? AND closed_at = 0`, now, id, org)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// BlockShare takes a link down for good. ok is false for an unknown name.
func (s *Store) BlockShare(ctx context.Context, name string, now int64) (Share, bool, error) {
	if _, err := s.exec(ctx, `UPDATE shares SET blocked_at = ? WHERE name = ? AND blocked_at = 0`, now, name); err != nil {
		return Share{}, false, err
	}
	return s.ShareByName(ctx, name)
}

// AddShareBytes adds one tunnel's traffic to the share's totals.
func (s *Store) AddShareBytes(ctx context.Context, id string, in, out int64) error {
	_, err := s.exec(ctx, `UPDATE shares SET bytes_in = bytes_in + ?, bytes_out = bytes_out + ? WHERE id = ?`, in, out, id)
	return err
}
