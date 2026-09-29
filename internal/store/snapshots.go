package store

// Named snapshots: a registry that gives a template_builds row a name and a
// version. The build row stays the owner of status, size and readiness; this
// table holds only what the control plane needs *before* it touches the
// artifact store — which runtime and which size a restore is pinned to, and
// which sandbox the capture came from.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
)

// maxSnapshotName is the grammar a caller may name a snapshot with
// (sandbox.ValidLabel). Deliberately narrower than a template id: the name is
// concatenated into a template id, so anything a template id cannot hold
// cannot be a snapshot name.
const maxSnapshotName = 40

// ErrSnapshotName means the name is not a legal snapshot name.
var ErrSnapshotName = errors.New("store: a snapshot name must be 1-40 characters of a-z, 0-9 and -, starting with a letter or digit")

// ValidSnapshotName reports whether name is a legal snapshot name.
func ValidSnapshotName(name string) error {
	if !sandbox.ValidLabel(name, maxSnapshotName) {
		return fmt.Errorf("%w: %q", ErrSnapshotName, name)
	}
	return nil
}

// SnapshotTemplateID is the template id every version of one named snapshot is
// published under. Riding an ordinary template id is what lets placement, the
// artifact index and the node's fetch path serve a snapshot with no new
// concepts (see the change's design notes).
//
// The org is hashed rather than spelled out because org ids are opaque and may
// contain '-': "snap-a-b-c" would otherwise be both org "a" name "b-c" and org
// "a-b" name "c", and one org could resolve another org's snapshot. The hash is
// a fixed-width field, so the split is unambiguous by construction.
func SnapshotTemplateID(orgID, name string) string {
	if orgID == "" {
		orgID = "default"
	}
	sum := sha256.Sum256([]byte(orgID))
	return "snap-" + hex.EncodeToString(sum[:4]) + "-" + name
}

// SnapshotBuildID is the build id a snapshot version publishes under. The
// version is zero-padded into it on purpose.
//
// ReadyBuildForTemplate resolves a template with `ORDER BY ready_at DESC,
// build_id DESC`, and ready_at has one-second granularity: two versions of one
// name saved inside the same second fall through to the build_id tie-break,
// which for a random id is a coin toss. Measured on the reference node — the
// control plane promised version 2 while ReadyBuildForTemplate answered version
// 1's build. Padding the version in makes the lexicographic tie-break agree
// with version order by construction, for the node and the control plane alike,
// with no extra query and no schema change.
func SnapshotBuildID(version int) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("bld-s%010d-%s", version, hex.EncodeToString(b[:]))
}

// Snapshot is one row of snapshots: one saved version of a name.
type Snapshot struct {
	ID      string `json:"id"`
	OrgID   string `json:"org_id"`
	Name    string `json:"name"`
	Version int    `json:"version"`
	BuildID string `json:"build_id"`
	// Runtime is the runtime the capture was taken on; a restore is pinned to it.
	Runtime string `json:"runtime"`
	// CPUFeatures is the dispatch-relevant instruction set of the host that
	// took the capture, sorted. A frozen guest has already chosen its code
	// paths from CPUID, so a node without all of these kills it with SIGILL on
	// the first instruction it does not implement; placement reads this to pick
	// a node that can actually run the image. Empty on a capture taken before
	// it was recorded, and on Docker, whose capture carries no memory at all.
	CPUFeatures []string `json:"cpu_features,omitempty"`
	// Template is the source sandbox's template, whose rootfs a Firecracker
	// restore still needs on disk.
	Template      string `json:"template"`
	VCPU          int    `json:"vcpu"`
	MemMB         int    `json:"mem_mb"`
	SourceSandbox string `json:"source_sandbox"`
	CreatedAt     int64  `json:"created_at"`
	DeletedAt     int64  `json:"deleted_at,omitempty"`
}

// Kind reports whether this snapshot carries guest memory or only a disk image.
// Firecracker captures RAM; Docker commits a filesystem and nothing else, and
// every response says which so a caller never assumes a memory image exists on
// both runtimes.
func (s Snapshot) Kind() string {
	if s.Runtime == "docker" {
		return "disk"
	}
	return "memory"
}

// SnapshotView is a snapshot joined to the build that holds its bytes.
type SnapshotView struct {
	Snapshot
	// Status is the build's status (building, ready, failed), or "deleted"
	// once the version's objects have been purged.
	Status  string `json:"status"`
	Size    int64  `json:"size"`
	ReadyAt int64  `json:"ready_at,omitempty"`
}

// ErrSnapshotNotFound means this org has no snapshot of that name.
var ErrSnapshotNotFound = errors.New("store: no such snapshot")

// SaveSnapshot records a new version of a name and fills in ID and Version.
// The version is the prior maximum plus one; the unique index is the arbiter,
// so two concurrent saves of one name cannot both claim a version.
func (s *Store) SaveSnapshot(ctx context.Context, snap *Snapshot) error {
	if err := ValidSnapshotName(snap.Name); err != nil {
		return err
	}
	if snap.OrgID == "" {
		snap.OrgID = "default"
	}
	// Two attempts: one for the ordinary case, one for losing the race on the
	// unique index. A third collision means something other than a race.
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		var max sql.NullInt64
		if err = s.queryRow(ctx,
			`SELECT MAX(version) FROM snapshots WHERE org_id = ? AND name = ?`,
			snap.OrgID, snap.Name).Scan(&max); err != nil {
			return fmt.Errorf("store: read the current version of snapshot %q: %w", snap.Name, err)
		}
		snap.Version = int(max.Int64) + 1
		if snap.ID == "" {
			snap.ID = newSnapshotID()
		}
		// Derived from the version, so the build id sorts in version order; see
		// SnapshotBuildID. A retry after losing the unique-index race gets a new
		// version and therefore a new build id, which is the point.
		snap.BuildID = SnapshotBuildID(snap.Version)
		_, err = s.exec(ctx,
			`INSERT INTO snapshots (id, org_id, name, version, build_id, runtime, template,
			                        vcpu, mem_mb, source_sandbox, created_at, deleted_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`,
			snap.ID, snap.OrgID, snap.Name, snap.Version, snap.BuildID, snap.Runtime,
			snap.Template, snap.VCPU, snap.MemMB, snap.SourceSandbox, snap.CreatedAt)
		if err == nil {
			return nil
		}
		snap.ID, snap.BuildID = "", ""
	}
	return fmt.Errorf("store: save snapshot %q: %w", snap.Name, err)
}

func newSnapshotID() string {
	var b [9]byte
	_, _ = rand.Read(b[:])
	return "snap-" + hex.EncodeToString(b[:])
}

// UpdateSnapshotCapture fills in what only the capture knows: which template
// the machine was booted from and the shape it must be restored into. Written
// after the objects are stored, so a row can never promise a size for bytes
// that are not there.
func (s *Store) UpdateSnapshotCapture(ctx context.Context, id, template string, vcpu, memMB int, cpuFeatures []string) error {
	if _, err := s.exec(ctx,
		`UPDATE snapshots SET template = ?, vcpu = ?, mem_mb = ?, cpu_features = ? WHERE id = ?`,
		template, vcpu, memMB, strings.Join(cpuFeatures, ","), id); err != nil {
		return fmt.Errorf("store: record what snapshot %q was captured from: %w", id, err)
	}
	return nil
}

// snapshotSelect is the join every read shares: the registry row plus the
// build's status and size. A version whose row is deleted reports "deleted"
// whatever the build still says, since its objects are gone.
const snapshotSelect = `
	SELECT s.id, s.org_id, s.name, s.version, s.build_id, s.runtime, s.template,
	       s.vcpu, s.mem_mb, s.source_sandbox, s.created_at, s.deleted_at, s.cpu_features,
	       COALESCE(b.status, ''), COALESCE(b.total_size, 0), COALESCE(b.ready_at, 0)
	  FROM snapshots s LEFT JOIN template_builds b ON b.build_id = s.build_id`

func scanSnapshots(rows *sql.Rows) ([]SnapshotView, error) {
	defer rows.Close()
	out := make([]SnapshotView, 0, 8)
	for rows.Next() {
		v, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// rowScanner is what both *sql.Row and *sql.Rows offer.
type rowScanner interface{ Scan(...any) error }

func scanSnapshot(r rowScanner) (SnapshotView, error) {
	var v SnapshotView
	var features string
	if err := r.Scan(&v.ID, &v.OrgID, &v.Name, &v.Version, &v.BuildID, &v.Runtime, &v.Template,
		&v.VCPU, &v.MemMB, &v.SourceSandbox, &v.CreatedAt, &v.DeletedAt, &features,
		&v.Status, &v.Size, &v.ReadyAt); err != nil {
		return v, err
	}
	// Split on non-empty only: strings.Split("", ",") is []string{""}, and one
	// empty feature name would refuse every node.
	if features != "" {
		v.CPUFeatures = strings.Split(features, ",")
	}
	if v.DeletedAt > 0 {
		v.Status = "deleted"
	}
	return v, nil
}

// SnapshotsForOrg lists an org's snapshot versions, newest first. Deleted
// versions are included: a name's history is what makes "v2 replaced v1" legible.
func (s *Store) SnapshotsForOrg(ctx context.Context, orgID string) ([]SnapshotView, error) {
	if orgID == "" {
		orgID = "default"
	}
	rows, err := s.query(ctx, snapshotSelect+
		` WHERE s.org_id = ? ORDER BY s.name, s.version DESC`, orgID)
	if err != nil {
		return nil, fmt.Errorf("store: list snapshots for %q: %w", orgID, err)
	}
	return scanSnapshots(rows)
}

// SnapshotVersions lists every version of one name, newest first.
func (s *Store) SnapshotVersions(ctx context.Context, orgID, name string) ([]SnapshotView, error) {
	if orgID == "" {
		orgID = "default"
	}
	rows, err := s.query(ctx, snapshotSelect+
		` WHERE s.org_id = ? AND s.name = ? ORDER BY s.version DESC`, orgID, name)
	if err != nil {
		return nil, fmt.Errorf("store: list versions of snapshot %q: %w", name, err)
	}
	return scanSnapshots(rows)
}

// LatestReadySnapshot resolves a name to the version a create would boot.
//
// The ordering is ready_at DESC, build_id DESC — deliberately the same tuple
// ReadyBuildForTemplate uses rather than version DESC. The node resolves the
// template id independently of this query, and two versions can share a
// ready_at second; ordering differently here would let the control plane
// promise version N while the node booted version N-1's bytes.
func (s *Store) LatestReadySnapshot(ctx context.Context, orgID, name string) (SnapshotView, error) {
	if orgID == "" {
		orgID = "default"
	}
	row := s.queryRow(ctx, snapshotSelect+
		` WHERE s.org_id = ? AND s.name = ? AND s.deleted_at = 0
		    AND b.status = 'ready' AND b.ready_at > 0
		  ORDER BY b.ready_at DESC, b.build_id DESC LIMIT 1`, orgID, name)
	v, err := scanSnapshot(row)
	if errors.Is(err, sql.ErrNoRows) {
		return v, fmt.Errorf("%w: %s", ErrSnapshotNotFound, name)
	}
	if err != nil {
		return v, fmt.Errorf("store: resolve snapshot %q: %w", name, err)
	}
	return v, nil
}

// SupersededSnapshots returns the live versions of a name other than keep,
// for the purge that runs once a new version is ready. Marking is the caller's
// job: it must not forget a version whose objects it has already removed, so
// MarkSnapshotDeleted takes the ids this returns.
func (s *Store) SupersededSnapshots(ctx context.Context, orgID, name, keepID string) ([]SnapshotView, error) {
	if orgID == "" {
		orgID = "default"
	}
	rows, err := s.query(ctx, snapshotSelect+
		` WHERE s.org_id = ? AND s.name = ? AND s.deleted_at = 0 AND s.id <> ?
		  ORDER BY s.version DESC`, orgID, name, keepID)
	if err != nil {
		return nil, fmt.Errorf("store: list superseded versions of %q: %w", name, err)
	}
	return scanSnapshots(rows)
}

// MarkSnapshotDeleted records that a version's objects are gone. The row stays.
func (s *Store) MarkSnapshotDeleted(ctx context.Context, id string, now int64) error {
	if now <= 0 {
		return fmt.Errorf("store: deleting snapshot %q needs a timestamp", id)
	}
	if _, err := s.exec(ctx,
		`UPDATE snapshots SET deleted_at = ? WHERE id = ? AND deleted_at = 0`, now, id); err != nil {
		return fmt.Errorf("store: mark snapshot %q deleted: %w", id, err)
	}
	return nil
}

// DeleteSnapshot marks every live version of a name deleted and returns them,
// so the caller can purge their objects. ErrSnapshotNotFound when the org has
// no live version of that name — which is also what a foreign org sees.
func (s *Store) DeleteSnapshot(ctx context.Context, orgID, name string, now int64) ([]SnapshotView, error) {
	live, err := s.SupersededSnapshots(ctx, orgID, name, "")
	if err != nil {
		return nil, err
	}
	if len(live) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrSnapshotNotFound, name)
	}
	for _, v := range live {
		if err := s.MarkSnapshotDeleted(ctx, v.ID, now); err != nil {
			return nil, err
		}
	}
	return live, nil
}

// DeleteBuild drops a build from the index and returns the objects that were
// recorded for it, for the caller to remove from the object store. The index
// goes first: a ready row pointing at absent bytes fails every create for that
// template, while a missing row just falls through to the next ready build.
//
// The row is removed rather than marked, because template_builds.status only
// admits building/ready/failed and a deleted build is none of those; the
// snapshots row keeps the build id, so the history is still readable.
func (s *Store) DeleteBuild(ctx context.Context, buildID string) ([]ArtifactObject, error) {
	objs, err := s.ArtifactObjects(ctx, buildID)
	if err != nil {
		return nil, err
	}
	if _, err := s.exec(ctx, `DELETE FROM template_builds WHERE build_id = ?`, buildID); err != nil {
		return nil, fmt.Errorf("store: drop build %q: %w", buildID, err)
	}
	if _, err := s.exec(ctx, `DELETE FROM artifacts WHERE build_id = ?`, buildID); err != nil {
		return nil, fmt.Errorf("store: drop artifact rows of %q: %w", buildID, err)
	}
	return objs, nil
}
