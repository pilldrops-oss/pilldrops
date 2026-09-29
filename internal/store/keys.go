package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrInvalidKey is returned when a key is unknown or revoked.
var ErrInvalidKey = errors.New("invalid api key")

// ErrKeyNotFound is returned when a key id is not one of the caller's org's keys.
// It deliberately does not distinguish "missing" from "another tenant's", so the
// revoke endpoint cannot be used to probe other orgs' key ids.
var ErrKeyNotFound = errors.New("key not found")

// Key is a stored API key row (never contains the plaintext).
type Key struct {
	ID    string
	Name  string
	OrgID string
	Role  string // "user" (default) or "operator" (may reach /v1/admin/*)
}

func randomToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func hashKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// CreateKey mints a new "user" key. See CreateKeyWithRole for operator keys.
func (s *Store) CreateKey(ctx context.Context, name, orgID string) (Key, string, error) {
	return s.CreateKeyWithRole(ctx, name, orgID, "user")
}

// CreateKeyWithRole mints a key with the given role and returns the row plus the
// one-time plaintext; only the SHA-256 hash is stored.
func (s *Store) CreateKeyWithRole(ctx context.Context, name, orgID, role string) (Key, string, error) {
	if orgID == "" {
		orgID = "default"
	}
	if role == "" {
		role = "user"
	}
	plaintext := "sf_live_" + randomToken(24)
	id := "key_" + randomToken(8)
	_, err := s.exec(ctx,
		`INSERT INTO api_keys (id, name, key_hash, org_id, role, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, name, hashKey(plaintext), orgID, role, time.Now().Unix(),
	)
	if err != nil {
		return Key{}, "", fmt.Errorf("insert key: %w", err)
	}
	return Key{ID: id, Name: name, OrgID: orgID, Role: role}, plaintext, nil
}

// EnsureKeyWithValue idempotently registers a caller-supplied plaintext key
// (e.g. a node's provisioned NODE_API_KEY). Only the hash is stored.
func (s *Store) EnsureKeyWithValue(ctx context.Context, plaintext, orgID, role string) error {
	if orgID == "" {
		orgID = "default"
	}
	if role == "" {
		role = "user"
	}
	_, err := s.exec(ctx,
		`INSERT INTO api_keys (id, name, key_hash, org_id, role, created_at)
		 VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (key_hash) DO NOTHING`,
		"key_"+randomToken(8), "node", hashKey(plaintext), orgID, role, time.Now().Unix(),
	)
	return err
}

// ValidateKey looks up a plaintext key by hash and touches last_used.
// Returns ErrInvalidKey if the key is unknown or revoked.
func (s *Store) ValidateKey(ctx context.Context, plaintext string) (Key, error) {
	var k Key
	var revoked sql.NullInt64
	err := s.queryRow(ctx,
		`SELECT id, name, org_id, role, revoked_at FROM api_keys WHERE key_hash = ?`,
		hashKey(plaintext),
	).Scan(&k.ID, &k.Name, &k.OrgID, &k.Role, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return Key{}, ErrInvalidKey
	}
	if err != nil {
		return Key{}, err
	}
	if revoked.Valid {
		return Key{}, ErrInvalidKey
	}
	// last_used is telemetry; do not make the request wait on the write.
	go func(id string, ts int64) {
		_, _ = s.exec(ctx, `UPDATE api_keys SET last_used = ? WHERE id = ?`, ts, id)
	}(k.ID, time.Now().Unix())
	return k, nil
}

// KeyInfo is safe key metadata for listing (never the secret).
type KeyInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Masked   string `json:"masked_key"`
	OrgID    string `json:"org_id"`
	LastUsed int64  `json:"last_used"`
	Created  int64  `json:"created_at"`
}

// ListKeys returns non-revoked keys for an org (never the plaintext secret).
func (s *Store) ListKeys(ctx context.Context, orgID string) ([]KeyInfo, error) {
	if orgID == "" {
		orgID = "default"
	}
	rows, err := s.query(ctx,
		`SELECT id, name, org_id, COALESCE(last_used,0), created_at
		 FROM api_keys WHERE org_id = ? AND revoked_at IS NULL ORDER BY created_at DESC`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KeyInfo
	for rows.Next() {
		var k KeyInfo
		if err := rows.Scan(&k.ID, &k.Name, &k.OrgID, &k.LastUsed, &k.Created); err != nil {
			return nil, err
		}
		k.Masked = "sf_live_" + strings.Repeat("*", 4) + k.ID[len(k.ID)-4:]
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeKey marks one of orgID's keys revoked. The org is part of the WHERE
// clause so a tenant cannot revoke another tenant's key by id.
func (s *Store) RevokeKey(ctx context.Context, id, orgID string) error {
	if orgID == "" {
		orgID = "default"
	}
	res, err := s.exec(ctx,
		`UPDATE api_keys SET revoked_at = ? WHERE id = ? AND org_id = ?`,
		time.Now().Unix(), id, orgID,
	)
	if err != nil {
		return err
	}
	// Only a confirmed zero means "not this org's key".
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrKeyNotFound
	}
	return nil
}
