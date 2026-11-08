package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ErrVolumeAttached is an attach or delete of a volume a sandbox holds.
var ErrVolumeAttached = errors.New("the volume is attached to a sandbox; destroy that sandbox first")

// Volume is a disk that outlives the sandbox it is attached to.
type Volume struct {
	ID         string `json:"id"`
	OrgID      string `json:"-"`
	Name       string `json:"name"`
	SizeMB     int    `json:"size_mb"`
	NodeID     string `json:"node_id,omitempty"`
	AttachedTo string `json:"attached_to,omitempty"`
	CreatedAt  int64  `json:"created_at"`
}

const volumeCols = `id, org_id, name, size_mb, node_id, attached_to, created_at`

func scanVolume(sc interface{ Scan(...any) error }) (Volume, error) {
	var v Volume
	err := sc.Scan(&v.ID, &v.OrgID, &v.Name, &v.SizeMB, &v.NodeID, &v.AttachedTo, &v.CreatedAt)
	return v, err
}

// CreateVolume records a new, unattached volume.
func (s *Store) CreateVolume(ctx context.Context, v Volume) error {
	if v.CreatedAt == 0 {
		v.CreatedAt = time.Now().Unix()
	}
	_, err := s.exec(ctx, `INSERT INTO volumes (`+volumeCols+`) VALUES (?, ?, ?, ?, ?, '', ?)`,
		v.ID, v.OrgID, v.Name, v.SizeMB, v.NodeID, v.CreatedAt)
	return err
}

// GetVolume reads one volume; ok is false when there is none.
func (s *Store) GetVolume(ctx context.Context, id string) (Volume, bool, error) {
	v, err := scanVolume(s.queryRow(ctx, `SELECT `+volumeCols+` FROM volumes WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Volume{}, false, nil
	}
	return v, err == nil, err
}

// ListVolumes lists an org's volumes, newest first.
func (s *Store) ListVolumes(ctx context.Context, orgID string) ([]Volume, error) {
	rows, err := s.query(ctx, `SELECT `+volumeCols+` FROM volumes WHERE org_id = ? ORDER BY created_at DESC`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Volume{}
	for rows.Next() {
		v, err := scanVolume(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// AttachVolume marks a volume held by sandboxID, refusing one another sandbox holds.
// One conditional UPDATE, so two creates naming the same volume cannot both win.
func (s *Store) AttachVolume(ctx context.Context, id, sandboxID string) error {
	res, err := s.exec(ctx, `UPDATE volumes SET attached_to = ? WHERE id = ? AND attached_to = ''`, sandboxID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrVolumeAttached
	}
	return nil
}

// VolumesAttachedTo lists the volumes a sandbox holds.
func (s *Store) VolumesAttachedTo(ctx context.Context, sandboxID string) ([]string, error) {
	rows, err := s.query(ctx, `SELECT id FROM volumes WHERE attached_to = ? ORDER BY id`, sandboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// DetachVolumes releases every volume a sandbox holds.
func (s *Store) DetachVolumes(ctx context.Context, sandboxID string) error {
	_, err := s.exec(ctx, `UPDATE volumes SET attached_to = '' WHERE attached_to = ?`, sandboxID)
	return err
}

// DeleteVolume removes an unattached volume's record.
func (s *Store) DeleteVolume(ctx context.Context, id string) error {
	res, err := s.exec(ctx, `DELETE FROM volumes WHERE id = ? AND attached_to = ''`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, ok, _ := s.GetVolume(ctx, id); ok {
			return ErrVolumeAttached
		}
	}
	return nil
}
