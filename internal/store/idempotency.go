package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// getIdempotent returns the raw cell stored under (orgID, idemKey), which may
// be the in-progress placeholder. Unexported: callers must use ReserveIdempotent
// to decide whether to do work and IdempotentResult to read the outcome.
func (s *Store) getIdempotent(ctx context.Context, orgID, idemKey string) ([]byte, bool, error) {
	if orgID == "" {
		orgID = "default"
	}
	var resp string
	err := s.queryRow(ctx,
		`SELECT response FROM idempotency WHERE org_id = ? AND idem_key = ?`, orgID, idemKey,
	).Scan(&resp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return []byte(resp), true, nil
}

// ReserveIdempotent atomically claims (orgID, idemKey) and reports whether this
// call was the one that claimed it. The database picks exactly one winner;
// a get-then-put would let two concurrent payment webhooks both grant.
func (s *Store) ReserveIdempotent(ctx context.Context, orgID, idemKey string, response []byte) (bool, error) {
	if orgID == "" {
		orgID = "default"
	}
	res, err := s.exec(ctx,
		`INSERT INTO idempotency (org_id, idem_key, response, at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (org_id, idem_key) DO NOTHING`,
		orgID, idemKey, string(response), time.Now().Unix(),
	)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return false, err
	} else if n > 0 {
		return true, nil
	}

	// An unfinished claim older than the window is abandoned (the process died
	// mid-flight) and may be adopted. The adoption is conditional on the row
	// still looking abandoned so two retries cannot both win.
	res, err = s.exec(ctx,
		`UPDATE idempotency SET at = ? WHERE org_id = ? AND idem_key = ? AND response = ? AND at < ?`,
		time.Now().Unix(), orgID, idemKey, claimInProgress, time.Now().Unix()-claimStaleSecs,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// claimInProgress marks a claim whose effect has not been applied yet.
const claimInProgress = "processing"

// claimStaleSecs is how long an unfinished claim is respected before a retry may adopt it.
const claimStaleSecs = 300

// IdempotentResult reports the finished response stored under (orgID, idemKey).
// done is false both for an in-flight claim and for a key never seen.
func (s *Store) IdempotentResult(ctx context.Context, orgID, idemKey string) (body []byte, done bool, err error) {
	b, ok, err := s.getIdempotent(ctx, orgID, idemKey)
	if err != nil || !ok {
		return nil, false, err
	}
	if string(b) == claimInProgress {
		return nil, false, nil
	}
	return b, true, nil
}

// FinishIdempotent replaces the reserved placeholder with the final response.
func (s *Store) FinishIdempotent(ctx context.Context, orgID, idemKey string, response []byte) error {
	if orgID == "" {
		orgID = "default"
	}
	_, err := s.exec(ctx,
		`UPDATE idempotency SET response = ? WHERE org_id = ? AND idem_key = ?`,
		string(response), orgID, idemKey,
	)
	return err
}

// ReleaseIdempotent gives a claimed key back after the guarded effect failed,
// so the sender's retry gets a fresh attempt.
func (s *Store) ReleaseIdempotent(ctx context.Context, orgID, idemKey string) error {
	if orgID == "" {
		orgID = "default"
	}
	_, err := s.exec(ctx,
		`DELETE FROM idempotency WHERE org_id = ? AND idem_key = ?`, orgID, idemKey,
	)
	return err
}
