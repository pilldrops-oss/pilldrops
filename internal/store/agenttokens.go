package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/auth/aat"
)

// Agent Access Token storage. Crypto and token format live in internal/aat so
// nothing inside the guest can mint a credential.

// Token lookup errors. All three are only reachable after the hash check, so
// distinguishing them is not an oracle.
var (
	ErrInvalidAgentToken = errors.New("invalid agent access token")
	ErrAgentTokenRevoked = errors.New("agent access token revoked")
	ErrAgentTokenExpired = errors.New("agent access token expired")
)

// AgentToken is a stored token row. It never contains the secret.
type AgentToken struct {
	ID         string
	SandboxID  string
	OrgID      string
	Scopes     aat.Set
	Label      string
	CreatedBy  string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	RevokedAt  time.Time
	LastUsedAt time.Time
	LastUsedIP string
	// Supersedes is the id this token replaced, set by rotation.
	Supersedes string
}

// Revoked reports whether the token has been revoked.
func (t AgentToken) Revoked() bool { return !t.RevokedAt.IsZero() }

// Active reports whether the token would authenticate right now.
func (t AgentToken) Active(now time.Time) bool {
	return !t.Revoked() && now.Before(t.ExpiresAt)
}

// maxTokenTTL bounds how long a delegated credential may live.
const maxTokenTTL = 30 * 24 * time.Hour

// defaultTokenTTL applies when the caller does not choose.
const defaultTokenTTL = 7 * 24 * time.Hour

// AgentTokenRequest describes a token to mint.
type AgentTokenRequest struct {
	SandboxID string
	OrgID     string
	Scopes    aat.Set
	Label     string
	// CreatedBy records the org key id that minted this, for audit.
	CreatedBy string
	TTL       time.Duration
	// supersedes is set internally by rotation only.
	supersedes string
}

// CreateAgentToken mints a token bound to one sandbox and returns the row plus the
// one-time plaintext. Only the hash is stored.
func (s *Store) CreateAgentToken(ctx context.Context, req AgentTokenRequest) (AgentToken, string, error) {
	if req.SandboxID == "" {
		return AgentToken{}, "", errors.New("agent token: sandbox id required")
	}
	if len(req.Scopes) == 0 {
		return AgentToken{}, "", errors.New("agent token: at least one scope required")
	}
	if req.OrgID == "" {
		req.OrgID = "default"
	}
	ttl := req.TTL
	if ttl <= 0 {
		ttl = defaultTTLFor(req.Scopes)
	}
	if cap := maxTTLFor(req.Scopes); ttl > cap {
		// Clamp rather than reject; the response carries the actual expiry.
		ttl = cap
	}

	tok := aat.Mint()
	now := time.Now()
	row := AgentToken{
		ID: tok.ID, SandboxID: req.SandboxID, OrgID: req.OrgID,
		Scopes: req.Scopes, Label: req.Label, CreatedBy: req.CreatedBy,
		CreatedAt: now.Truncate(time.Second), ExpiresAt: now.Add(ttl).Truncate(time.Second),
		Supersedes: req.supersedes,
	}
	_, err := s.exec(ctx,
		`INSERT INTO agent_tokens
		   (id, sandbox_id, org_id, secret_sha256, scopes, label, created_by,
		    created_at, expires_at, supersedes)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.ID, row.SandboxID, row.OrgID, tok.SecretSHA256, req.Scopes.Encode(),
		row.Label, row.CreatedBy, row.CreatedAt.Unix(), row.ExpiresAt.Unix(), row.Supersedes,
	)
	if err != nil {
		return AgentToken{}, "", fmt.Errorf("insert agent token: %w", err)
	}
	return row, tok.Secret, nil
}

const agentTokenCols = `id, sandbox_id, org_id, scopes, label, created_by,
	created_at, expires_at, revoked_at, last_used_at, last_used_ip, supersedes`

func scanAgentToken(sc rowScanner) (AgentToken, error) {
	var (
		t                             AgentToken
		scopes                        string
		created, expires              int64
		revoked, lastUsed             sql.NullInt64
		lastIP, label, by, supersedes sql.NullString
	)
	err := sc.Scan(&t.ID, &t.SandboxID, &t.OrgID, &scopes, &label, &by,
		&created, &expires, &revoked, &lastUsed, &lastIP, &supersedes)
	if err != nil {
		return AgentToken{}, err
	}
	t.Scopes = aat.DecodeSet(scopes)
	t.Label, t.CreatedBy, t.LastUsedIP, t.Supersedes = label.String, by.String, lastIP.String, supersedes.String
	t.CreatedAt, t.ExpiresAt = time.Unix(created, 0), time.Unix(expires, 0)
	if revoked.Valid {
		t.RevokedAt = time.Unix(revoked.Int64, 0)
	}
	if lastUsed.Valid {
		t.LastUsedAt = time.Unix(lastUsed.Int64, 0)
	}
	return t, nil
}

// ValidateAgentToken authenticates a plaintext token. The hash comparison must
// run before the revoked and expiry checks so error detail never leaks token state
// to someone who only guessed an id.
func (s *Store) ValidateAgentToken(ctx context.Context, plaintext string) (AgentToken, error) {
	id, err := aat.ParseID(plaintext)
	if err != nil {
		return AgentToken{}, ErrInvalidAgentToken
	}
	var storedHash string
	row := s.queryRow(ctx,
		`SELECT `+agentTokenCols+`, secret_sha256 FROM agent_tokens WHERE id = ?`, id)

	var (
		t                             AgentToken
		scopes                        string
		created, expires              int64
		revoked, lastUsed             sql.NullInt64
		lastIP, label, by, supersedes sql.NullString
	)
	err = row.Scan(&t.ID, &t.SandboxID, &t.OrgID, &scopes, &label, &by,
		&created, &expires, &revoked, &lastUsed, &lastIP, &supersedes, &storedHash)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentToken{}, ErrInvalidAgentToken
	}
	if err != nil {
		return AgentToken{}, err
	}
	if !aat.VerifyHash(plaintext, storedHash) {
		return AgentToken{}, ErrInvalidAgentToken
	}

	t.Scopes = aat.DecodeSet(scopes)
	t.Label, t.CreatedBy, t.LastUsedIP, t.Supersedes = label.String, by.String, lastIP.String, supersedes.String
	t.CreatedAt, t.ExpiresAt = time.Unix(created, 0), time.Unix(expires, 0)
	if revoked.Valid {
		t.RevokedAt = time.Unix(revoked.Int64, 0)
		return t, ErrAgentTokenRevoked
	}
	if lastUsed.Valid {
		t.LastUsedAt = time.Unix(lastUsed.Int64, 0)
	}
	if !time.Now().Before(t.ExpiresAt) {
		return t, ErrAgentTokenExpired
	}
	return t, nil
}

// claimFirstTouch reports whether this process should perform the first
// last_used write for a token, at most once per token per process. Without it a
// never-used token has no timestamp for the sampling guard and writes on every request.
func (s *Store) claimFirstTouch(id string) bool {
	s.firstTouchMu.Lock()
	defer s.firstTouchMu.Unlock()
	if s.firstTouch == nil {
		s.firstTouch = map[string]struct{}{}
	}
	if _, seen := s.firstTouch[id]; seen {
		return false
	}
	// Dropping the memo on overflow costs at most one extra write per token.
	if len(s.firstTouch) >= maxFirstTouchTracked {
		s.firstTouch = map[string]struct{}{}
	}
	s.firstTouch[id] = struct{}{}
	return true
}

// maxFirstTouchTracked bounds the first-touch memo.
const maxFirstTouchTracked = 4096

// touchInterval is how coarsely last_used is recorded; a polling viewer must
// not turn into a steady write load on one row.
const touchInterval = 5 * time.Minute

// TouchAgentToken records use, at most once per touchInterval per token.
// Fire-and-forget: never on the request's latency path.
func (s *Store) TouchAgentToken(ctx context.Context, t AgentToken, ip string) {
	now := time.Now()
	// A never-used token is due exactly once per process, not on every request.
	if !t.LastUsedAt.IsZero() && now.Sub(t.LastUsedAt) < touchInterval {
		return
	}
	if t.LastUsedAt.IsZero() && !s.claimFirstTouch(t.ID) {
		return
	}
	go func(id, ip string, ts int64) {
		_, _ = s.exec(ctx,
			`UPDATE agent_tokens SET last_used_at = ?, last_used_ip = ? WHERE id = ?`,
			ts, ip, id)
	}(t.ID, ip, now.Unix())
}

// ListAgentTokens returns every token for a sandbox, newest first, revoked and
// expired ones included for audit.
func (s *Store) ListAgentTokens(ctx context.Context, sandboxID string) ([]AgentToken, error) {
	rows, err := s.query(ctx,
		`SELECT `+agentTokenCols+` FROM agent_tokens WHERE sandbox_id = ? ORDER BY created_at DESC, id`,
		sandboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentToken
	for rows.Next() {
		t, err := scanAgentToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetAgentToken loads one token by id, scoped to its sandbox.
func (s *Store) GetAgentToken(ctx context.Context, sandboxID, id string) (AgentToken, error) {
	row := s.queryRow(ctx,
		`SELECT `+agentTokenCols+` FROM agent_tokens WHERE id = ? AND sandbox_id = ?`,
		id, sandboxID)
	t, err := scanAgentToken(row)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentToken{}, ErrInvalidAgentToken
	}
	return t, err
}

// RevokeAgentToken revokes one token. Idempotent; a repeat keeps the original timestamp.
func (s *Store) RevokeAgentToken(ctx context.Context, sandboxID, id string) error {
	res, err := s.exec(ctx,
		`UPDATE agent_tokens SET revoked_at = ? WHERE id = ? AND sandbox_id = ? AND revoked_at IS NULL`,
		time.Now().Unix(), id, sandboxID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Unknown, another sandbox's, or already revoked; only the last is success.
		if t, err := s.GetAgentToken(ctx, sandboxID, id); err == nil && t.Revoked() {
			return nil
		}
		return ErrInvalidAgentToken
	}
	return nil
}

// RevokeAgentTokensForSandbox revokes every live token for a sandbox and returns
// how many it revoked. Called on destroy so nothing authenticates against a reused id.
func (s *Store) RevokeAgentTokensForSandbox(ctx context.Context, sandboxID string) (int, error) {
	res, err := s.exec(ctx,
		`UPDATE agent_tokens SET revoked_at = ? WHERE sandbox_id = ? AND revoked_at IS NULL`,
		time.Now().Unix(), sandboxID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// RevokeAgentTokensWithScope revokes the live tokens for a sandbox that grant
// scope, returning the ids revoked. Filtered in Go, not with a SQL LIKE, so a
// future scope sharing a prefix cannot over-revoke.
func (s *Store) RevokeAgentTokensWithScope(ctx context.Context, sandboxID string, scope aat.Scope) ([]string, error) {
	all, err := s.ListAgentTokens(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var revoked []string
	for _, t := range all {
		if !t.Active(now) || !t.Scopes.Has(scope) {
			continue
		}
		if err := s.RevokeAgentToken(ctx, sandboxID, t.ID); err != nil {
			return revoked, err
		}
		revoked = append(revoked, t.ID)
	}
	return revoked, nil
}

// rotationOverlap is how long the old token keeps working after a rotation so
// in-flight requests still succeed.
const rotationOverlap = 5 * time.Minute

// RotateAgentToken issues a replacement with the same sandbox, scopes and label,
// and shortens the old token's life to the overlap window. The old token is
// expired rather than revoked so the audit trail shows a rotation, not a revocation.
func (s *Store) RotateAgentToken(ctx context.Context, sandboxID, id, createdBy string) (AgentToken, string, error) {
	old, err := s.GetAgentToken(ctx, sandboxID, id)
	if err != nil {
		return AgentToken{}, "", err
	}
	if old.Revoked() {
		// Rotating a revoked token would resurrect deliberately removed access.
		return AgentToken{}, "", ErrAgentTokenRevoked
	}

	ttl := time.Until(old.ExpiresAt)
	if ttl <= 0 {
		ttl = defaultTokenTTL
	}
	fresh, secret, err := s.CreateAgentToken(ctx, AgentTokenRequest{
		SandboxID: sandboxID, OrgID: old.OrgID, Scopes: old.Scopes,
		Label: old.Label, CreatedBy: createdBy, TTL: ttl, supersedes: old.ID,
	})
	if err != nil {
		return AgentToken{}, "", err
	}

	cutoff := time.Now().Add(rotationOverlap).Unix()
	// Never extend the old token's expiry.
	if _, err := s.exec(ctx,
		`UPDATE agent_tokens SET expires_at = ? WHERE id = ? AND sandbox_id = ? AND expires_at > ?`,
		cutoff, old.ID, sandboxID, cutoff,
	); err != nil {
		// The replacement exists; return it so the caller does not mint a third.
		return fresh, secret, fmt.Errorf("rotated, but the previous token's expiry was not shortened: %w", err)
	}
	return fresh, secret, nil
}

// PurgeExpiredAgentTokens deletes rows that expired or were revoked before cutoff,
// and returns how many went. Never run automatically: this destroys the audit trail.
func (s *Store) PurgeExpiredAgentTokens(ctx context.Context, cutoff time.Time) (int, error) {
	res, err := s.exec(ctx,
		`DELETE FROM agent_tokens
		  WHERE (expires_at < ? AND revoked_at IS NULL) OR (revoked_at IS NOT NULL AND revoked_at < ?)`,
		cutoff.Unix(), cutoff.Unix())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// Per-purpose token lifetimes.
const (
	// consoleMaxTTL is a ceiling, not a default: a console token grants a shell.
	consoleMaxTTL = 15 * time.Minute
	// shareDefaultTTL is what a share link gets when the caller does not choose.
	shareDefaultTTL = 24 * time.Hour
)

// defaultTTLFor picks the lifetime for a token whose caller did not choose one.
func defaultTTLFor(scopes aat.Set) time.Duration {
	if scopes.Has(aat.ScopeConsole) {
		return consoleMaxTTL
	}
	// Exactly read and nothing else is the shape a share link mints.
	if len(scopes) == 1 && scopes.Has(aat.ScopeRead) {
		return shareDefaultTTL
	}
	return defaultTokenTTL
}

// maxTTLFor is the ceiling for a token with these scopes.
func maxTTLFor(scopes aat.Set) time.Duration {
	if scopes.Has(aat.ScopeConsole) {
		return consoleMaxTTL
	}
	return maxTokenTTL
}
