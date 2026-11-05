package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// The artifact index: the object store owns an artifact's bytes, the database
// owns which builds exist and may be booted. A build is 'ready' only after its
// bytes are published and hashed, and the API shape enforces the order:
// BeginTemplateBuild (cannot be ready), RecordArtifactObject per object, then
// MarkTemplateBuildReady. There is no FOREIGN KEY between template_builds and
// artifacts because SQLite would silently ignore it.
//
// The writers here are control-plane only; the node role has SELECT and nothing
// else, since a node can already PUT bytes into the shared store.

// TemplateBuild is one row of template_builds.
type TemplateBuild struct {
	BuildID    string `json:"build_id"`
	TemplateID string `json:"template_id"`
	OrgID      string `json:"org_id"`
	// Status is one of building, ready, failed; a fourth value is a migration.
	Status             string `json:"status"`
	RootfsSize         int64  `json:"rootfs_size"`
	RootfsSHA256       string `json:"rootfs_sha256"`
	TotalSize          int64  `json:"total_size"`
	KernelVersion      string `json:"kernel_version,omitempty"`
	FirecrackerVersion string `json:"firecracker_version,omitempty"`
	EnvdVersion        string `json:"envd_version,omitempty"`
	CreatedAt          int64  `json:"created_at"`
	ReadyAt            int64  `json:"ready_at"`
}

// Ready reports whether this build may be booted.
func (b TemplateBuild) Ready() bool { return b.Status == "ready" }

// ArtifactObject is one row of artifacts: bytes that are already in the store.
type ArtifactObject struct {
	BuildID string `json:"build_id"`
	// Kind is the artifact name from internal/artifact (rootfs, rootfs.header, metadata.json).
	Kind       string `json:"kind"`
	Key        string `json:"key"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	StoreKind  string `json:"store_kind"`
	UploadedAt int64  `json:"uploaded_at"`
}

// ErrBuildNotBuilding is returned by MarkTemplateBuildReady when the row is
// missing or no longer 'building'.
var ErrBuildNotBuilding = errors.New("store: build is not in the 'building' state")

// BeginTemplateBuild records a build that is being published. It writes no
// size, checksum or ready_at, so the row can never be bootable.
func (s *Store) BeginTemplateBuild(ctx context.Context, b TemplateBuild) error {
	if b.BuildID == "" || b.TemplateID == "" {
		return fmt.Errorf("store: a build needs both a build id and a template id")
	}
	org := b.OrgID
	if org == "" {
		org = "default"
	}
	_, err := s.exec(ctx,
		`INSERT INTO template_builds (build_id, template_id, org_id, status, created_at)
		 VALUES (?, ?, ?, 'building', ?)`,
		b.BuildID, b.TemplateID, org, b.CreatedAt)
	if err != nil {
		return fmt.Errorf("store: begin build %q: %w", b.BuildID, err)
	}
	return nil
}

// RecordArtifactObject records one object that is already in the store.
// Upserts on (build_id, kind) so a re-publish describes the bytes there now.
func (s *Store) RecordArtifactObject(ctx context.Context, a ArtifactObject) error {
	if a.StoreKind == "" {
		return fmt.Errorf("store: artifact %q does not say which store holds it", a.Key)
	}
	_, err := s.exec(ctx,
		`INSERT INTO artifacts (build_id, kind, key, size, sha256, store_kind, uploaded_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (build_id, kind) DO UPDATE SET
		   key = excluded.key, size = excluded.size, sha256 = excluded.sha256,
		   store_kind = excluded.store_kind, uploaded_at = excluded.uploaded_at`,
		a.BuildID, a.Kind, a.Key, a.Size, a.SHA256, a.StoreKind, a.UploadedAt)
	if err != nil {
		return fmt.Errorf("store: record artifact %q: %w", a.Key, err)
	}
	return nil
}

// MarkTemplateBuildReady is the last step of a publish and the only one that
// can make a build bootable. Guarded on status='building' so a retried publish
// cannot re-bless objects a concurrent publisher has since overwritten.
func (s *Store) MarkTemplateBuildReady(ctx context.Context, b TemplateBuild) error {
	if b.RootfsSize <= 0 || len(b.RootfsSHA256) != 64 {
		// The CHECK constraint would refuse this too; this error names what is missing.
		return fmt.Errorf("store: build %q cannot be marked ready with size %d and checksum %q — "+
			"both are facts that only exist after the bytes have been written and hashed",
			b.BuildID, b.RootfsSize, b.RootfsSHA256)
	}
	if b.ReadyAt <= 0 {
		return fmt.Errorf("store: build %q needs a ready timestamp", b.BuildID)
	}
	res, err := s.exec(ctx,
		`UPDATE template_builds
		    SET status = 'ready', ready_at = ?, rootfs_size = ?, rootfs_sha256 = ?,
		        total_size = ?, kernel_version = ?, firecracker_version = ?, envd_version = ?
		  WHERE build_id = ? AND status = 'building'`,
		b.ReadyAt, b.RootfsSize, b.RootfsSHA256, b.TotalSize,
		b.KernelVersion, b.FirecrackerVersion, b.EnvdVersion, b.BuildID)
	if err != nil {
		return fmt.Errorf("store: mark build %q ready: %w", b.BuildID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: mark build %q ready: %w", b.BuildID, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %q", ErrBuildNotBuilding, b.BuildID)
	}
	return nil
}

// FailTemplateBuild marks a 'building' row failed. Best effort: a killed
// process cannot call it, so nothing may depend on it. Never un-readies a live build.
func (s *Store) FailTemplateBuild(ctx context.Context, buildID string) error {
	_, err := s.exec(ctx,
		`UPDATE template_builds SET status = 'failed' WHERE build_id = ? AND status = 'building'`,
		buildID)
	return err
}

// TemplateBuilds lists a template's builds, newest first, every status
// included. An empty org means every org and is for operator callers only.
func (s *Store) TemplateBuilds(ctx context.Context, templateID, org string, limit int) ([]TemplateBuild, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT build_id, template_id, org_id, status, rootfs_size, rootfs_sha256, total_size,
	             kernel_version, firecracker_version, envd_version, created_at, ready_at
	        FROM template_builds
	       WHERE template_id = ?`
	args := []any{templateID}
	if org != "" {
		q += ` AND org_id = ?`
		args = append(args, org)
	}
	q += ` ORDER BY ready_at DESC, created_at DESC, build_id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list builds for %q: %w", templateID, err)
	}
	defer rows.Close()
	out := make([]TemplateBuild, 0, 8)
	for rows.Next() {
		var b TemplateBuild
		if err := rows.Scan(&b.BuildID, &b.TemplateID, &b.OrgID, &b.Status, &b.RootfsSize,
			&b.RootfsSHA256, &b.TotalSize, &b.KernelVersion, &b.FirecrackerVersion,
			&b.EnvdVersion, &b.CreatedAt, &b.ReadyAt); err != nil {
			return nil, fmt.Errorf("store: scan build: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// TemplateBuildByID returns one build, or nil when there is none.
func (s *Store) TemplateBuildByID(ctx context.Context, buildID string) (*TemplateBuild, error) {
	var b TemplateBuild
	err := s.queryRow(ctx,
		`SELECT build_id, template_id, org_id, status, rootfs_size, rootfs_sha256, total_size,
		        kernel_version, firecracker_version, envd_version, created_at, ready_at
		   FROM template_builds WHERE build_id = ?`, buildID,
	).Scan(&b.BuildID, &b.TemplateID, &b.OrgID, &b.Status, &b.RootfsSize, &b.RootfsSHA256,
		&b.TotalSize, &b.KernelVersion, &b.FirecrackerVersion, &b.EnvdVersion, &b.CreatedAt, &b.ReadyAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: read build %q: %w", buildID, err)
	}
	return &b, nil
}

// ArtifactObjects lists the objects recorded for a build, in key order.
func (s *Store) ArtifactObjects(ctx context.Context, buildID string) ([]ArtifactObject, error) {
	rows, err := s.query(ctx,
		`SELECT build_id, kind, key, size, sha256, store_kind, uploaded_at
		   FROM artifacts WHERE build_id = ? ORDER BY key`, buildID)
	if err != nil {
		return nil, fmt.Errorf("store: list artifacts of %q: %w", buildID, err)
	}
	defer rows.Close()
	out := make([]ArtifactObject, 0, 4)
	for rows.Next() {
		var a ArtifactObject
		if err := rows.Scan(&a.BuildID, &a.Kind, &a.Key, &a.Size, &a.SHA256,
			&a.StoreKind, &a.UploadedAt); err != nil {
			return nil, fmt.Errorf("store: scan artifact: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CustomTemplate is one custom image an org built: a template with a ready build.
type CustomTemplate struct {
	TemplateID string
	TotalSize  int64
	ReadyAt    int64
}

// CustomTemplatesForOrg lists org's built images, whose sf- ids customImageTag
// makes. Snapshot captures share the table and are not listed.
func (s *Store) CustomTemplatesForOrg(ctx context.Context, org string) ([]CustomTemplate, error) {
	rows, err := s.query(ctx, `SELECT template_id, MAX(total_size), MAX(ready_at)
	        FROM template_builds
	       WHERE org_id = ? AND status = 'ready' AND template_id LIKE 'sf-%'
	       GROUP BY template_id ORDER BY template_id`, org)
	if err != nil {
		return nil, fmt.Errorf("store: list custom templates: %w", err)
	}
	defer rows.Close()
	var out []CustomTemplate
	for rows.Next() {
		var t CustomTemplate
		if err := rows.Scan(&t.TemplateID, &t.TotalSize, &t.ReadyAt); err != nil {
			return nil, fmt.Errorf("store: scan custom template: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
