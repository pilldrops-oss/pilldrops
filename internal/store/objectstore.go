package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

const objectStoreKey = "object_store"

// ObjectStore is the bucket an operator saved for the control plane and every
// node to share, so it is entered once instead of in each machine's environment.
type ObjectStore struct {
	Endpoint  string `json:"endpoint"`
	Bucket    string `json:"bucket"`
	Region    string `json:"region"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

// ErrObjectStoreNeedsKey refuses to keep bucket credentials in plaintext.
var ErrObjectStoreNeedsKey = errors.New(
	"refusing to save object store credentials unencrypted: set SANDBOXD_SECRET_KEY (64 hex chars) " +
		"on the control plane and the nodes")

// GetObjectStore returns the saved bucket, or nil when none is saved.
func (s *Store) GetObjectStore(ctx context.Context) (*ObjectStore, error) {
	var enc string
	err := s.queryRow(ctx, `SELECT value FROM settings WHERE key = ?`, objectStoreKey).Scan(&enc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// Sealed with the key nodes share, not the vault key: nodes read this row.
	plain, err := s.crypt.open(enc)
	if err != nil {
		return nil, err
	}
	var o ObjectStore
	if err := json.Unmarshal([]byte(plain), &o); err != nil {
		return nil, err
	}
	return &o, nil
}

// SetObjectStore saves the bucket, replacing any previous one.
func (s *Store) SetObjectStore(ctx context.Context, o ObjectStore) error {
	if s.crypt == nil {
		return ErrObjectStoreNeedsKey
	}
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	enc, err := s.crypt.seal(string(b))
	if err != nil {
		return err
	}
	_, err = s.exec(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		objectStoreKey, enc)
	return err
}

// DeleteObjectStore removes the saved bucket; a missing one is not an error.
func (s *Store) DeleteObjectStore(ctx context.Context) error {
	_, err := s.exec(ctx, `DELETE FROM settings WHERE key = ?`, objectStoreKey)
	return err
}
