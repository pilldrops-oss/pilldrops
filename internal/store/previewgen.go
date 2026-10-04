package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
)

// previewGenKey is the settings row holding a sandbox's preview revocation
// counter.
// ponytail: settings is a KV table and this is one row per sandbox that ever
// flipped; it moves to a sandboxes column the next time that table migrates.
func previewGenKey(id string) string { return "preview_gen/" + id }

// PreviewGeneration returns how many times a sandbox's preview access has
// been flipped (visibility or public), 0 when never. A token bound to an older
// count never validates again, so flipping back cannot revive it.
func (s *Store) PreviewGeneration(ctx context.Context, id string) (int64, error) {
	var v string
	err := s.queryRow(ctx, `SELECT value FROM settings WHERE key = ?`, previewGenKey(id)).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(v, 10, 64)
}

// BumpPreviewGeneration advances the counter in one statement, so two
// replicas flipping at once both count.
func (s *Store) BumpPreviewGeneration(ctx context.Context, id string) error {
	_, err := s.exec(ctx,
		`INSERT INTO settings (key, value) VALUES (?, '1')
		 ON CONFLICT(key) DO UPDATE SET value = CAST(CAST(settings.value AS INTEGER) + 1 AS TEXT)`,
		previewGenKey(id))
	return err
}

// deletePreviewGeneration drops the counter with its sandbox.
func (s *Store) deletePreviewGeneration(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `DELETE FROM settings WHERE key = ?`, previewGenKey(id))
	return err
}
