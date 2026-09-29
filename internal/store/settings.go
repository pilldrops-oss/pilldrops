package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

const imageFlagsKey = "image_flags"

// ImageFlags are admin-curated per-template overrides. The zero value means no
// override, which callers treat as published and not featured.
type ImageFlags struct {
	Published   *bool  `json:"published,omitempty"`
	Featured    bool   `json:"featured,omitempty"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

// GetImageFlags returns the stored per-template flag map, never nil.
func (s *Store) GetImageFlags(ctx context.Context) (map[string]ImageFlags, error) {
	var val string
	err := s.queryRow(ctx, `SELECT value FROM settings WHERE key = ?`, imageFlagsKey).Scan(&val)
	if errors.Is(err, sql.ErrNoRows) {
		return map[string]ImageFlags{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get image flags: %w", err)
	}
	var m map[string]ImageFlags
	if err := json.Unmarshal([]byte(val), &m); err != nil {
		return nil, fmt.Errorf("decode image flags: %w", err)
	}
	if m == nil {
		m = map[string]ImageFlags{}
	}
	return m, nil
}

// SetImageFlags merges flags for one template id and returns the result.
func (s *Store) SetImageFlags(ctx context.Context, id string, upd ImageFlags) (ImageFlags, error) {
	m, err := s.GetImageFlags(ctx)
	if err != nil {
		return ImageFlags{}, err
	}
	cur := m[id]
	if upd.Published != nil {
		cur.Published = upd.Published
	}
	cur.Featured = upd.Featured
	if upd.Name != "" {
		cur.Name = upd.Name
	}
	if upd.Description != "" {
		cur.Description = upd.Description
	}
	m[id] = cur
	b, err := json.Marshal(m)
	if err != nil {
		return ImageFlags{}, fmt.Errorf("encode image flags: %w", err)
	}
	_, err = s.exec(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		imageFlagsKey, string(b))
	if err != nil {
		return ImageFlags{}, fmt.Errorf("set image flags: %w", err)
	}
	return cur, nil
}
