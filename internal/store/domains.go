package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// A custom domain is a customer hostname pointed at the platform. The row is
// the whole feature: routing serves only 'active' rows, the verifier writes
// the status, and the TLS terminator's ask endpoint reads the same rows, so a
// host we never verified can never get a certificate.

// Domain is one customer hostname. A wildcard row ("*.vms.example.com") has
// no SandboxID: its label names the sandbox at request time.
type Domain struct {
	Host      string `json:"host"`
	OrgID     string `json:"org_id"`
	SandboxID string `json:"sandbox_id,omitempty"`
	Port      int    `json:"port,omitempty"`
	Wildcard  bool   `json:"wildcard,omitempty"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	CreatedAt int64  `json:"created_at"`
	CheckedAt int64  `json:"checked_at,omitempty"`
	// LegacyTargetSeenAt is nonzero while the row's DNS names a legacy sandbox
	// domain: when that was first seen, then when it last served traffic.
	LegacyTargetSeenAt int64 `json:"legacy_target_seen_at,omitempty"`
}

// ErrDomainTaken means another org already attached the host. Hosts are global:
// two orgs cannot both own the DNS name, and the org holding a VERIFIED row
// keeps it. An unverified claim expires — see DomainClaimGrace.
var ErrDomainTaken = errors.New("host is already attached by another org")

// DomainClaimGrace is how long a claim that has never routed holds the host
// against every other org. Nothing proves ownership at claim time — the CNAME
// is the proof, and the verifier is what checks it — so without an expiry one
// tenant could permanently reserve a competitor's hostname in a shared
// deployment by typing it in once.
//
// Seven days, measured from created_at, because that column is the only
// evidence the row carries: a row that verified once and has since broken also
// reads 'pending', so a shorter window would let a squatter take an
// established customer's host after a DNS outage. Seven days of continuously
// failing DNS is abandonment, not an outage.
//
// ponytail: created_at is a proxy for "never verified". A verified_at column
// would say it exactly and let this shrink to about 72 h; it needs a
// migration, which is why it is not here.
const DomainClaimGrace = 7 * 24 * 60 * 60 // seconds

const domainCols = `host, org_id, sandbox_id, port, wildcard, status, error, created_at, checked_at, legacy_target_seen_at`

// PutDomain inserts the row, or re-points the org's own existing row and sends
// it back to 'pending'. The ownership test is inside the statement, so two orgs
// racing on one host cannot both win: the loser gets ErrDomainTaken.
//
// A stale unverified claim is takeable too: another org may have the host once
// the row has been pending since it was created, DomainClaimGrace ago. Taking
// it wins nothing but the right to try — the host still routes only once the
// verifier sees the customer's CNAME pointing here.
func (s *Store) PutDomain(ctx context.Context, d Domain) error {
	d.Host = strings.ToLower(strings.TrimSpace(d.Host))
	if d.OrgID == "" {
		d.OrgID = "default"
	}
	res, err := s.exec(ctx,
		`INSERT INTO custom_domains (`+domainCols+`)
		 VALUES (?, ?, ?, ?, ?, 'pending', '', ?, 0, 0)
		 ON CONFLICT(host) DO UPDATE SET
		   org_id = excluded.org_id,
		   sandbox_id = excluded.sandbox_id, port = excluded.port, wildcard = excluded.wildcard,
		   status = 'pending', error = '', checked_at = 0,
		   -- Only a change of holder restarts the clock. Re-pointing your own
		   -- row must not extend a squat by another grace period.
		   created_at = CASE WHEN custom_domains.org_id = excluded.org_id
		                     THEN custom_domains.created_at ELSE excluded.created_at END
		 WHERE custom_domains.org_id = excluded.org_id
		    OR (custom_domains.status = 'pending' AND custom_domains.created_at <= ?)`,
		d.Host, d.OrgID, d.SandboxID, d.Port, d.Wildcard, d.CreatedAt,
		d.CreatedAt-DomainClaimGrace,
	)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrDomainTaken
	}
	return nil
}

// GetDomain returns one row by host. ok=false when the host is not attached.
func (s *Store) GetDomain(ctx context.Context, host string) (Domain, bool, error) {
	var d Domain
	err := s.queryRow(ctx, `SELECT `+domainCols+` FROM custom_domains WHERE host = ?`,
		strings.ToLower(strings.TrimSpace(host)),
	).Scan(&d.Host, &d.OrgID, &d.SandboxID, &d.Port, &d.Wildcard, &d.Status, &d.Error, &d.CreatedAt, &d.CheckedAt,
		&d.LegacyTargetSeenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Domain{}, false, nil
	}
	return d, err == nil, err
}

// ListDomainsByOrg returns an org's rows, host-ordered. Never nil.
func (s *Store) ListDomainsByOrg(ctx context.Context, orgID string) ([]Domain, error) {
	if orgID == "" {
		orgID = "default"
	}
	return s.queryDomains(ctx, `WHERE org_id = ? ORDER BY host`, orgID)
}

// DeleteDomain removes one of the org's rows. ok=false when the org has no such
// host, which the caller answers as a 404 rather than a 403.
func (s *Store) DeleteDomain(ctx context.Context, orgID, host string) (bool, error) {
	res, err := s.exec(ctx, `DELETE FROM custom_domains WHERE host = ? AND org_id = ?`,
		strings.ToLower(strings.TrimSpace(host)), orgID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DomainsDue returns the rows the verifier should resolve now: every pending
// row, plus active rows last checked before staleBefore.
func (s *Store) DomainsDue(ctx context.Context, staleBefore int64, limit int) ([]Domain, error) {
	return s.queryDomains(ctx,
		`WHERE status = 'pending' OR checked_at <= ? ORDER BY checked_at LIMIT ?`, staleBefore, limit)
}

// ActiveWildcardDomains returns every active wildcard row, for the ingress
// suffix match. Small by construction: one row per customer org domain.
func (s *Store) ActiveWildcardDomains(ctx context.Context) ([]Domain, error) {
	// Bound as a parameter, not a literal: SQLite stores 0/1 and Postgres a boolean.
	return s.queryDomains(ctx, `WHERE wildcard = ? AND status = 'active'`, true)
}

// SetDomainStatus records a verification result. errText is the resolver's own
// text, shown to the customer, and is cleared when the host verifies.
func (s *Store) SetDomainStatus(ctx context.Context, host, status, errText string, checkedAt int64) error {
	_, err := s.exec(ctx, `UPDATE custom_domains SET status = ?, error = ?, checked_at = ? WHERE host = ?`,
		status, errText, checkedAt, strings.ToLower(host))
	return err
}

// queryDomains runs a custom_domains SELECT. where is appended verbatim, so it
// must be a constant, never caller data.
func (s *Store) queryDomains(ctx context.Context, where string, args ...any) ([]Domain, error) {
	return collectRows(ctx, s, `SELECT `+domainCols+` FROM custom_domains `+where, args,
		func(rows *sql.Rows, d *Domain) error {
			return rows.Scan(&d.Host, &d.OrgID, &d.SandboxID, &d.Port, &d.Wildcard,
				&d.Status, &d.Error, &d.CreatedAt, &d.CheckedAt, &d.LegacyTargetSeenAt)
		})
}
