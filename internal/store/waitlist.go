package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// WaitlistEntry is one "notify me" signup.
type WaitlistEntry struct {
	Email     string `json:"email"`
	Source    string `json:"source"` // e.g. "pricing", "billing"
	Note      string `json:"note"`
	CreatedAt int64  `json:"created_at"` // unix seconds of the first signup
}

// maxWaitlistEmailLen is the RFC 5321 address cap.
const maxWaitlistEmailLen = 254

// normalizeWaitlistEmail trims and lowercases; the check is deliberately loose.
func normalizeWaitlistEmail(email string) (string, error) {
	e := strings.ToLower(strings.TrimSpace(email))
	if e == "" {
		return "", fmt.Errorf("email is required")
	}
	if len(e) > maxWaitlistEmailLen {
		return "", fmt.Errorf("email is too long")
	}
	if !strings.Contains(e, "@") {
		return "", fmt.Errorf("email is not valid")
	}
	return e, nil
}

// AddWaitlist records a signup keyed by normalized email. A repeat refreshes
// source and note but keeps the first created_at.
func (s *Store) AddWaitlist(ctx context.Context, email, source, note string, now int64) error {
	e, err := normalizeWaitlistEmail(email)
	if err != nil {
		return err
	}
	_, err = s.exec(ctx,
		`INSERT INTO waitlist (email, source, note, created_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (email) DO UPDATE SET source = excluded.source, note = excluded.note`,
		e, source, note, now,
	)
	return err
}

// ListWaitlist returns signups newest first. limit outside [1,1000] becomes 200.
func (s *Store) ListWaitlist(ctx context.Context, limit int) ([]WaitlistEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	return collectRows(ctx, s,
		`SELECT email, source, note, created_at FROM waitlist ORDER BY created_at DESC LIMIT ?`,
		[]any{limit},
		func(rows *sql.Rows, e *WaitlistEntry) error {
			return rows.Scan(&e.Email, &e.Source, &e.Note, &e.CreatedAt)
		})
}
