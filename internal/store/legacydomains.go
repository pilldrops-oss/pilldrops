package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// A legacy sandbox domain is one previews moved off (PORTAL_LEGACY_SANDBOX_DOMAINS).
// Its preview hosts redirect while anyone uses them, and a custom domain whose
// DNS still names it keeps working while it gets traffic. These two clocks
// decide for how long.

// LegacyUseWindow is how long a legacy domain, or a custom domain's legacy
// CNAME, keeps working after its last use. Every use pushes the end out.
const LegacyUseWindow = 90 * 24 * time.Hour

// LegacyWriteEvery bounds how often one replica records a use of the same
// domain or row, so traffic never becomes a write per request.
const LegacyWriteEvery = 10 * time.Minute

// LegacyDomainLastUsed returns when domain last redirected anyone; ok=false
// when it never has.
func (s *Store) LegacyDomainLastUsed(ctx context.Context, domain string) (int64, bool, error) {
	var at int64
	err := s.queryRow(ctx, `SELECT last_used_at FROM legacy_domain_uses WHERE domain = ?`,
		strings.ToLower(domain)).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return at, err == nil, err
}

// TouchLegacyDomain records a use at at. It never moves the clock back, so a
// replica with a slow clock cannot shorten the window another one extended.
func (s *Store) TouchLegacyDomain(ctx context.Context, domain string, at int64) error {
	_, err := s.exec(ctx,
		`INSERT INTO legacy_domain_uses (domain, last_used_at) VALUES (?, ?)
		 ON CONFLICT(domain) DO UPDATE SET last_used_at = excluded.last_used_at
		 WHERE legacy_domain_uses.last_used_at < excluded.last_used_at`,
		strings.ToLower(domain), at)
	return err
}

// SetDomainLegacySeen is the verifier's record of which target a row's DNS
// names: 0 for the current one, else when the legacy one was first seen.
func (s *Store) SetDomainLegacySeen(ctx context.Context, host string, at int64) error {
	_, err := s.exec(ctx, `UPDATE custom_domains SET legacy_target_seen_at = ? WHERE host = ?`,
		at, strings.ToLower(host))
	return err
}

// TouchDomainLegacySeen records traffic on a row still on a legacy target. A
// row on the current target (0) is left alone, as is a later stamp.
func (s *Store) TouchDomainLegacySeen(ctx context.Context, host string, at int64) error {
	_, err := s.exec(ctx,
		`UPDATE custom_domains SET legacy_target_seen_at = ?
		 WHERE host = ? AND legacy_target_seen_at > 0 AND legacy_target_seen_at < ?`,
		at, strings.ToLower(host), at)
	return err
}
