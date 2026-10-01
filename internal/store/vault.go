package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
)

// VaultSecret is one org-scoped named entry, injected into sandboxes via
// secret_refs or auto_inject. Name doubles as the env var, so it must be a
// valid identifier. Kind is "secret" (sealed: list returns Last4 only) or
// "env" (readable: list returns Value).
type VaultSecret struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// AutoInject adds the entry to every sandbox the org creates.
	AutoInject bool `json:"auto_inject"`
	// Last4 lets the UI show a hint without exposing the key.
	Last4 string `json:"last4"`
	// Value is set only for kind "env", never for a sealed secret.
	Value string `json:"value,omitempty"`
	// Scope is which sandboxes this may be injected into: "all" (default),
	// "shared" (org-visible sandboxes only) or "private" (the creating key's
	// own private sandboxes only). CreatedBy is the key that stored it.
	Scope     string `json:"scope"`
	CreatedBy string `json:"created_by,omitempty"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// Secret scopes. "all" is the default and the behaviour before scopes existed.
const (
	ScopeAll     = "all"
	ScopeShared  = "shared"
	ScopePrivate = "private"
)

// ErrVaultScope is returned for a scope that is not one of the three.
var ErrVaultScope = errors.New(`scope must be "all", "shared" or "private"`)

// ValidScope normalises a requested scope; "" means "all".
func ValidScope(v string) (string, error) {
	switch v {
	case "":
		return ScopeAll, nil
	case ScopeAll, ScopeShared, ScopePrivate:
		return v, nil
	}
	return "", ErrVaultScope
}

const (
	VaultKindSecret = "secret"
	VaultKindEnv    = "env"
)

// ErrVaultName is returned for a name that is not a usable env identifier.
var ErrVaultName = errors.New("secret name must be an env identifier (letters, digits, underscore; max 64 chars; not PILLDROPS_/PD_)")

// PutSecret stores or replaces an entry, returning its safe representation.
// secret=false stores a readable "env" kind.
// createdBy is variadic so the callers that do not name a key are unchanged.
// An update keeps the original creator: a secret belongs to whoever put it
// there, not to whoever last rotated its value.
func (s *Store) PutSecret(ctx context.Context, orgID, name, value string, secret, autoInject bool, createdBy ...string) (VaultSecret, error) {
	return s.putSecret(ctx, orgID, name, value, secret, &autoInject, createdBy...)
}

// PutSecretKeepingAutoInject is PutSecret for a save that does not mention
// auto_inject: a new entry starts off, a replaced one keeps its setting. One
// statement, so a concurrent SetSecretAutoInject is never overwritten by a
// value read before it.
func (s *Store) PutSecretKeepingAutoInject(ctx context.Context, orgID, name, value string, secret bool, createdBy ...string) (VaultSecret, error) {
	return s.putSecret(ctx, orgID, name, value, secret, nil, createdBy...)
}

// putSecret stores an entry; autoInject nil leaves a replaced entry's flag as it is.
func (s *Store) putSecret(ctx context.Context, orgID, name, value string, secret bool, autoInject *bool, createdBy ...string) (VaultSecret, error) {
	name = strings.TrimSpace(name)
	if sandbox.ValidEnvName(name) != nil {
		return VaultSecret{}, ErrVaultName
	}
	if value == "" {
		return VaultSecret{}, errors.New("value is required")
	}
	enc, err := s.vaultSeal(value)
	if err != nil {
		return VaultSecret{}, err
	}
	last4 := value
	if len(last4) > 4 {
		last4 = last4[len(last4)-4:]
	}
	kind := VaultKindEnv
	if secret {
		kind = VaultKindSecret
	}
	auto := 0
	if autoInject != nil && *autoInject {
		auto = 1
	}
	owner := ""
	if len(createdBy) > 0 {
		owner = createdBy[0]
	}
	now := time.Now().Unix()
	setAuto := ", auto_inject = ?"
	args := []any{orgID, name, enc, last4, kind, auto, owner, now, now, enc, last4, kind, auto, now}
	if autoInject == nil {
		setAuto = ""
		args = append(args[:12], now)
	}
	if _, err := s.exec(ctx,
		`INSERT INTO vault_secrets (org_id, name, value, last4, kind, auto_inject, created_by, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(org_id, name) DO UPDATE SET value = ?, last4 = ?, kind = ?`+setAuto+`, updated_at = ?`,
		args...,
	); err != nil {
		return VaultSecret{}, err
	}
	stored := auto == 1
	if autoInject == nil {
		// Reported, not decided: the write above already left the flag alone.
		if err := s.queryRow(ctx, `SELECT auto_inject FROM vault_secrets WHERE org_id = ? AND name = ?`,
			orgID, name).Scan(&auto); err != nil {
			return VaultSecret{}, err
		}
		stored = auto == 1
	}
	out := VaultSecret{Name: name, Kind: kind, AutoInject: stored, Last4: last4,
		Scope: ScopeAll, CreatedBy: owner, CreatedAt: now, UpdatedAt: now}
	if kind == VaultKindEnv {
		out.Value = value
	}
	return out, nil
}

// ListSecrets returns an org's entries. Only kind "env" values are decrypted.
func (s *Store) ListSecrets(ctx context.Context, orgID string) ([]VaultSecret, error) {
	return collectRows(ctx, s,
		`SELECT name, value, last4, kind, auto_inject, scope, created_by, created_at, updated_at FROM vault_secrets
		 WHERE org_id = ? ORDER BY name`, []any{orgID},
		func(rows *sql.Rows, v *VaultSecret) error {
			var enc string
			var auto int
			if err := rows.Scan(&v.Name, &enc, &v.Last4, &v.Kind, &auto, &v.Scope, &v.CreatedBy, &v.CreatedAt, &v.UpdatedAt); err != nil {
				return err
			}
			v.AutoInject = auto == 1
			if v.Kind == VaultKindEnv {
				// Best effort: one undecryptable row must not fail the whole list.
				if plain, err := s.vaultOpen(enc); err == nil {
					v.Value = plain
				}
			}
			return nil
		})
}

// ListAutoInject returns the decrypted name→value of every auto_inject entry
// of an org, for the create path.
func (s *Store) ListAutoInject(ctx context.Context, orgID string) (map[string]string, error) {
	rows, err := s.query(ctx,
		// Only 'all'-scoped entries auto-inject: a scoped secret says which
		// sandboxes it may reach, and "every sandbox this org creates" is the
		// one answer that cannot honour it. A scoped secret is named explicitly.
		`SELECT name, value FROM vault_secrets WHERE org_id = ? AND auto_inject = 1 AND scope = 'all'`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, enc string
		if err := rows.Scan(&name, &enc); err != nil {
			return nil, err
		}
		plain, err := s.vaultOpen(enc)
		if err != nil {
			return nil, err
		}
		out[name] = plain
	}
	return out, rows.Err()
}

// SecretValue returns the decrypted value of one secret; ok is false when missing.
func (s *Store) SecretValue(ctx context.Context, orgID, name string) (string, bool, error) {
	var enc string
	err := s.queryRow(ctx,
		`SELECT value FROM vault_secrets WHERE org_id = ? AND name = ?`, orgID, name).Scan(&enc)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	plain, err := s.vaultOpen(enc)
	if err != nil {
		return "", false, err
	}
	return plain, true, nil
}

// SecretForInject returns a secret's value together with the scope and creator
// the injection rule needs. Separate from SecretValue so the callers that only
// want a value keep their one-line call.
func (s *Store) SecretForInject(ctx context.Context, orgID, name string) (value, scope, createdBy string, found bool, err error) {
	var enc string
	err = s.queryRow(ctx,
		`SELECT value, scope, created_by FROM vault_secrets WHERE org_id = ? AND name = ?`,
		orgID, name).Scan(&enc, &scope, &createdBy)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", false, nil
	}
	if err != nil {
		return "", "", "", false, err
	}
	plain, err := s.vaultOpen(enc)
	if err != nil {
		return "", "", "", false, err
	}
	if scope == "" {
		scope = ScopeAll
	}
	return plain, scope, createdBy, true, nil
}

// SetSecretScope changes which sandboxes a secret may reach. The value is
// untouched: a scope change is not a rotation. ok=false means no such secret.
func (s *Store) SetSecretScope(ctx context.Context, orgID, name, scope string) (bool, error) {
	scope, err := ValidScope(scope)
	if err != nil {
		return false, err
	}
	res, err := s.exec(ctx,
		`UPDATE vault_secrets SET scope = ?, updated_at = ? WHERE org_id = ? AND name = ?`,
		scope, time.Now().Unix(), orgID, name)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SetSecretAutoInject turns auto-injection on or off without touching the
// value, so a caller never has to re-type a credential to flip it.
// ok=false means no such secret.
func (s *Store) SetSecretAutoInject(ctx context.Context, orgID, name string, autoInject bool) (bool, error) {
	auto := 0
	if autoInject {
		auto = 1
	}
	res, err := s.exec(ctx,
		`UPDATE vault_secrets SET auto_inject = ?, updated_at = ? WHERE org_id = ? AND name = ?`,
		auto, time.Now().Unix(), orgID, name)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DeleteSecret removes a secret, reporting whether one existed.
func (s *Store) DeleteSecret(ctx context.Context, orgID, name string) (bool, error) {
	res, err := s.exec(ctx, `DELETE FROM vault_secrets WHERE org_id = ? AND name = ?`, orgID, name)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
