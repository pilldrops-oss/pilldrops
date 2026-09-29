package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// A sandbox's captures in object storage. The row is the index; the bytes live
// under object_key in whatever artifact store the node was configured with.

// Backup statuses.
const (
	BackupPending = "pending"
	BackupReady   = "ready"
	BackupFailed  = "failed"
)

// ErrBackupReferenced means another backup's parent_id points at this row, so
// deleting it would strand the child: a deduped capture has no bytes of its
// own and restores from its parent's object.
var ErrBackupReferenced = errors.New("backup is referenced by another backup")

// Backup is one capture of one sandbox.
type Backup struct {
	ID        string `json:"id"`
	OrgID     string `json:"org_id"`
	SandboxID string `json:"sandbox_id"`
	NodeID    string `json:"node_id,omitempty"`
	// Status is pending, ready or failed.
	Status string `json:"status"`
	// Kind is what the capture holds: "memory" (RAM and processes come back)
	// or "disk" (filesystem only). The engine decides, not the runtime's name.
	Kind string `json:"kind,omitempty"`
	// ObjectKey is where the bytes are. Empty on a deduped row, which reads its
	// parent's object.
	ObjectKey string `json:"-"`
	Bytes     int64  `json:"bytes"`
	SHA256    string `json:"sha256,omitempty"`
	// ParentID is the earlier backup this one is byte-identical to; empty when
	// this row owns its own object.
	ParentID  string `json:"parent_id,omitempty"`
	Error     string `json:"error,omitempty"`
	CreatedAt int64  `json:"created_at"`
	ReadyAt   int64  `json:"ready_at,omitempty"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
}

func newBackupID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "bkp_" + hex.EncodeToString(b), nil
}

const backupCols = `id, org_id, sandbox_id, node_id, status, kind, object_key, bytes, sha256, parent_id, error, created_at, ready_at, expires_at`

func scanBackup(sc rowScanner) (Backup, error) {
	var b Backup
	err := sc.Scan(&b.ID, &b.OrgID, &b.SandboxID, &b.NodeID, &b.Status, &b.Kind, &b.ObjectKey,
		&b.Bytes, &b.SHA256, &b.ParentID, &b.Error, &b.CreatedAt, &b.ReadyAt, &b.ExpiresAt)
	return b, err
}

// CreateBackup writes a pending row and returns it with its generated id.
// expiresAt of 0 means the deployment default decides, which the pruner applies.
func (s *Store) CreateBackup(ctx context.Context, b Backup) (Backup, error) {
	if b.OrgID == "" || b.SandboxID == "" {
		return Backup{}, fmt.Errorf("org and sandbox are required")
	}
	id, err := newBackupID()
	if err != nil {
		return Backup{}, err
	}
	b.ID, b.Status, b.CreatedAt = id, BackupPending, time.Now().Unix()
	_, err = s.exec(ctx,
		`INSERT INTO backups (id, org_id, sandbox_id, node_id, status, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		b.ID, b.OrgID, b.SandboxID, b.NodeID, b.Status, b.CreatedAt, b.ExpiresAt)
	if err != nil {
		return Backup{}, err
	}
	return b, nil
}

// SetBackupReady records what landed. A deduped capture passes parentID and an
// empty objectKey; it still carries the digest, so the next capture can dedupe
// against it without chasing the chain.
func (s *Store) SetBackupReady(ctx context.Context, id, kind, objectKey, sha256hex, parentID string, bytes int64) error {
	_, err := s.exec(ctx,
		`UPDATE backups SET status = ?, kind = ?, object_key = ?, sha256 = ?, parent_id = ?,
		 bytes = ?, ready_at = ?, error = '' WHERE id = ?`,
		BackupReady, kind, objectKey, sha256hex, parentID, bytes, time.Now().Unix(), id)
	return err
}

// SetBackupFailed records why a capture did not finish. The text is shown to
// the owner, so it must say what went wrong, not just that something did.
func (s *Store) SetBackupFailed(ctx context.Context, id, reason string) error {
	if len(reason) > 1024 {
		reason = reason[:1024]
	}
	_, err := s.exec(ctx, `UPDATE backups SET status = ?, error = ? WHERE id = ?`,
		BackupFailed, reason, id)
	return err
}

// BackupByID returns one backup scoped to its org.
func (s *Store) BackupByID(ctx context.Context, orgID, id string) (Backup, bool) {
	b, err := scanBackup(s.queryRow(ctx,
		`SELECT `+backupCols+` FROM backups WHERE id = ? AND org_id = ?`, id, orgID))
	if err != nil {
		return Backup{}, false
	}
	return b, true
}

// ListSandboxBackups returns a sandbox's backups, newest first.
func (s *Store) ListSandboxBackups(ctx context.Context, orgID, sandboxID string) ([]Backup, error) {
	return s.collectBackups(ctx,
		`SELECT `+backupCols+` FROM backups WHERE org_id = ? AND sandbox_id = ? ORDER BY created_at DESC, id DESC`,
		orgID, sandboxID)
}

// LastReadyBackup is the newest ready backup of a sandbox, for dedupe.
func (s *Store) LastReadyBackup(ctx context.Context, sandboxID string) (Backup, bool) {
	b, err := scanBackup(s.queryRow(ctx,
		`SELECT `+backupCols+` FROM backups WHERE sandbox_id = ? AND status = ?
		 ORDER BY ready_at DESC, id DESC LIMIT 1`, sandboxID, BackupReady))
	if err != nil {
		return Backup{}, false
	}
	return b, true
}

// PendingBackup reports an in-flight capture of a sandbox, which is what makes
// a second capture a 409 rather than two writers on one overlay.
func (s *Store) PendingBackup(ctx context.Context, sandboxID string) (Backup, bool) {
	b, err := scanBackup(s.queryRow(ctx,
		`SELECT `+backupCols+` FROM backups WHERE sandbox_id = ? AND status = ? LIMIT 1`,
		sandboxID, BackupPending))
	if err != nil {
		return Backup{}, false
	}
	return b, true
}

// BackupChildren returns the ids of backups whose parent_id is id.
func (s *Store) BackupChildren(ctx context.Context, id string) ([]string, error) {
	return collectRows(ctx, s, `SELECT id FROM backups WHERE parent_id = ?`, []any{id},
		func(rows *sql.Rows, child *string) error { return rows.Scan(child) })
}

// DeleteBackup removes a row, refusing one another backup depends on. The
// caller deletes the object first: a row without its object is invisible
// garbage, an object without its row is unreclaimable garbage.
func (s *Store) DeleteBackup(ctx context.Context, orgID, id string) error {
	children, err := s.BackupChildren(ctx, id)
	if err != nil {
		return err
	}
	if len(children) > 0 {
		return fmt.Errorf("%w: %s restores from it", ErrBackupReferenced, children[0])
	}
	res, err := s.exec(ctx, `DELETE FROM backups WHERE id = ? AND org_id = ?`, id, orgID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("backup %s not found", id)
	}
	return nil
}

// ExpiredBackups returns rows past their expiry that nothing else depends on,
// oldest first. defaultDays fills in for a row whose expires_at was never set;
// 0 there means those rows are kept forever.
//
// The child check is a NOT EXISTS rather than a second query so a capture that
// dedupes against a row mid-sweep cannot slip between the two.
func (s *Store) ExpiredBackups(ctx context.Context, now int64, defaultDays int, limit int) ([]Backup, error) {
	if limit <= 0 {
		limit = 100
	}
	cutoff := int64(0)
	if defaultDays > 0 {
		cutoff = now - int64(defaultDays)*24*3600
	}
	return s.collectBackups(ctx,
		`SELECT `+backupCols+` FROM backups
		 WHERE ((expires_at > 0 AND expires_at <= ?) OR (expires_at = 0 AND ? > 0 AND created_at <= ?))
		   AND NOT EXISTS (SELECT 1 FROM backups child WHERE child.parent_id = backups.id)
		 ORDER BY created_at LIMIT ?`,
		now, cutoff, cutoff, limit)
}

func (s *Store) collectBackups(ctx context.Context, q string, args ...any) ([]Backup, error) {
	return collectRows(ctx, s, q, args, scanInto(scanBackup))
}
