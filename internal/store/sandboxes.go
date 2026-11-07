package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// SandboxRecord is the durable subset of a sandbox, used to recover state after
// a daemon restart. Times are unix seconds.
type SandboxRecord struct {
	ID       string
	Template string
	// Name is an optional human label.
	Name string
	// Slug is the optional DNS label the sandbox is hosted under; '' is none.
	// Unique across the platform while the row exists (idx_sandboxes_slug).
	Slug  string
	OrgID string
	// NodeID is the owning node. Empty means unowned, which every node may
	// see and claim. See LoadSandboxesForNode.
	NodeID          string
	State           string
	Snapshot        string
	StartCmd        string
	PublicPort      int
	VCPU            int // chosen vCPU cores (0 = daemon default)
	MemMiB          int // chosen RAM in MiB (0 = daemon default)
	StorageMB       int // chosen writable-disk cap in MB (0 = uncapped)
	IdleTimeoutSecs int // idle standby override (0 = default, <0 = never)
	// HibernateTimeoutSecs is the disk-pause timer (0 = platform default, <0 = never).
	HibernateTimeoutSecs int
	// BackupRetentionSecs is how long this sandbox's backups are kept
	// (0 = BACKUPS_RETAIN_DAYS).
	BackupRetentionSecs int
	Env                 string // JSON object of injected env vars (replayed on resume)
	// IdentityHash is the sha256 of the sandbox's own pd_sb_ token; "" = none.
	IdentityHash string
	// Networks is a JSON array of the sandbox's network tags; "" and "[]" both
	// mean it joins no network, reaches no peer and is reached by none.
	Networks string
	// CreatedBy is the id of the API key that created or forked the sandbox;
	// "" for a row that predates visibility, which is then org-visible.
	CreatedBy string
	// Visibility is "org" (every key in the org sees it) or "private" (only
	// CreatedBy and operators). "" reads as "org", the pre-visibility default.
	Visibility string
	CreatedAt  int64
	ExpiresAt  int64
	UpdatedAt  int64
	// PauseArchive names the archived pause this row's snapshot is ("" for
	// none): the random id the archive's manifest carries. A rehome takes an
	// archive only when the ids match, so one left behind by an earlier pause
	// can never come back as the sandbox's state. Empty on any non-paused row.
	PauseArchive string
}

// ErrSlugTaken is returned by SaveSandbox when another live sandbox holds the slug.
var ErrSlugTaken = errors.New("slug is taken")

// slugConflict reports whether err is the unique-index violation on sandboxes.slug.
func slugConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "idx_sandboxes_slug") || strings.Contains(msg, "sandboxes.slug")
}

// SaveSandbox upserts a sandbox row. env is encrypted at rest when a secret
// key is configured. A slug another row holds returns ErrSlugTaken.
func (s *Store) SaveSandbox(ctx context.Context, r SandboxRecord) error {
	if r.Env == "" {
		r.Env = "{}"
	}
	if r.Networks == "" {
		r.Networks = "[]"
	}
	env, err := s.crypt.seal(r.Env)
	if err != nil {
		return err
	}
	r.Env = env
	// An empty incoming node_id leaves the stored owner alone, so a call site
	// that forgets to carry the id cannot un-own a live sandbox.
	_, err = s.exec(ctx,
		`INSERT INTO sandboxes (id, template, name, slug, org_id, node_id, state, snapshot, start_cmd, public_port, vcpu, mem_mib, storage_mb, idle_timeout, hibernate_timeout, backup_retention, env, identity_hash, networks, created_by, visibility, created_at, expires_at, updated_at, pause_archive)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   template=excluded.template, name=excluded.name, slug=excluded.slug, org_id=excluded.org_id, state=excluded.state,
		   node_id=CASE WHEN excluded.node_id = '' THEN sandboxes.node_id ELSE excluded.node_id END,
		   snapshot=excluded.snapshot, start_cmd=excluded.start_cmd, public_port=excluded.public_port,
		   vcpu=excluded.vcpu, mem_mib=excluded.mem_mib, storage_mb=excluded.storage_mb,
		   idle_timeout=excluded.idle_timeout, hibernate_timeout=excluded.hibernate_timeout,
		   backup_retention=excluded.backup_retention,
		   env=excluded.env, identity_hash=excluded.identity_hash, networks=excluded.networks,
		   created_by=CASE WHEN excluded.created_by = '' THEN sandboxes.created_by ELSE excluded.created_by END,
		   visibility=CASE WHEN excluded.visibility = '' THEN sandboxes.visibility ELSE excluded.visibility END,
		   expires_at=excluded.expires_at, updated_at=excluded.updated_at, pause_archive=excluded.pause_archive`,
		r.ID, r.Template, r.Name, r.Slug, r.OrgID, r.NodeID, r.State, r.Snapshot, r.StartCmd, r.PublicPort, r.VCPU, r.MemMiB, r.StorageMB, r.IdleTimeoutSecs, r.HibernateTimeoutSecs, r.BackupRetentionSecs, r.Env, r.IdentityHash, r.Networks, r.CreatedBy, r.Visibility, r.CreatedAt, r.ExpiresAt, r.UpdatedAt, r.PauseArchive,
	)
	if slugConflict(err) {
		return fmt.Errorf("%w: %q", ErrSlugTaken, r.Slug)
	}
	return err
}

// RecordPlacedSandbox inserts the row for a sandbox a node has just created,
// unless the node already wrote one: the node's own write is best-effort and
// only logged there, and without the row the control plane cannot list, move
// or route-fall-back to a sandbox that is running and billing.
func (s *Store) RecordPlacedSandbox(ctx context.Context, r SandboxRecord) error {
	_, err := s.exec(ctx,
		`INSERT INTO sandboxes (id, template, name, org_id, node_id, state, vcpu, mem_mib, created_by, visibility, created_at, expires_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO NOTHING`,
		r.ID, r.Template, r.Name, r.OrgID, r.NodeID, r.State, r.VCPU, r.MemMiB, r.CreatedBy, r.Visibility,
		r.CreatedAt, r.ExpiresAt, r.UpdatedAt,
	)
	return err
}

// SandboxBySlug returns the live sandbox holding slug; ok=false when none does.
func (s *Store) SandboxBySlug(ctx context.Context, slug string) (SandboxRecord, bool, error) {
	if slug == "" {
		return SandboxRecord{}, false, nil
	}
	return s.oneSandbox(ctx, `WHERE slug = ?`, slug)
}

// oneSandbox is querySandboxes for a where clause that names at most one row.
func (s *Store) oneSandbox(ctx context.Context, where string, args ...any) (SandboxRecord, bool, error) {
	rows, err := s.querySandboxes(ctx, where, args...)
	if err != nil || len(rows) == 0 {
		return SandboxRecord{}, false, err
	}
	return rows[0], true, nil
}

// maxClientTagLen caps the raw client tag stored on a sandbox.
const maxClientTagLen = 24

// SetSandboxAttribution records the classified source and raw client tag on
// a sandbox. An empty source leaves the stored one alone; client is always
// written, sanitized, even when empty.
func (s *Store) SetSandboxAttribution(ctx context.Context, id, source, client string) error {
	client = strings.ToLower(strings.TrimSpace(client))
	if len(client) > maxClientTagLen {
		// Trim to a rune boundary so the value stays valid UTF-8.
		cut := maxClientTagLen
		for cut > 0 && !utf8.RuneStart(client[cut]) {
			cut--
		}
		client = client[:cut]
	}
	_, err := s.exec(ctx,
		`UPDATE sandboxes SET source = CASE WHEN ? = '' THEN source ELSE ? END, client = ? WHERE id = ?`,
		source, source, client, id,
	)
	return err
}

// SetPublicPreview records whether the sandbox preview is reachable without a token.
// Stored in the DB because any replica may serve the preview request.
func (s *Store) SetPublicPreview(ctx context.Context, id string, public bool) error {
	_, err := s.exec(ctx, `UPDATE sandboxes SET public_preview = ? WHERE id = ?`, public, id)
	return err
}

// IsPublicPreview reports whether a sandbox's preview skips the token gate.
// A missing row answers false.
func (s *Store) IsPublicPreview(ctx context.Context, id string) (bool, error) {
	var public bool
	err := s.queryRow(ctx, `SELECT public_preview FROM sandboxes WHERE id = ?`, id).Scan(&public)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return public, nil
}

// sandboxCols is the column list every sandbox-record read shares.
const sandboxCols = `id, template, name, slug, org_id, node_id, state, snapshot, start_cmd, public_port, vcpu, mem_mib, storage_mb, idle_timeout, hibernate_timeout, backup_retention, env, identity_hash, networks, created_by, visibility, created_at, expires_at, updated_at, pause_archive`

// querySandboxes runs a sandbox SELECT and decrypts each row's env. where is
// appended verbatim, so it must be a constant, never caller data.
func (s *Store) querySandboxes(ctx context.Context, where string, args ...any) ([]SandboxRecord, error) {
	return collectRows(ctx, s, `SELECT `+sandboxCols+` FROM sandboxes `+where, args,
		func(rows *sql.Rows, r *SandboxRecord) error {
			if err := rows.Scan(&r.ID, &r.Template, &r.Name, &r.Slug, &r.OrgID, &r.NodeID, &r.State, &r.Snapshot,
				&r.StartCmd, &r.PublicPort, &r.VCPU, &r.MemMiB, &r.StorageMB, &r.IdleTimeoutSecs, &r.HibernateTimeoutSecs, &r.BackupRetentionSecs, &r.Env, &r.IdentityHash, &r.Networks, &r.CreatedBy, &r.Visibility, &r.CreatedAt, &r.ExpiresAt, &r.UpdatedAt, &r.PauseArchive); err != nil {
				return err
			}
			var err error
			r.Env, err = s.crypt.open(r.Env)
			return err
		})
}

// LoadSandboxes returns every persisted sandbox record across all nodes.
// Crash recovery uses LoadSandboxesForNode instead.
func (s *Store) LoadSandboxes(ctx context.Context) ([]SandboxRecord, error) {
	return s.querySandboxes(ctx, ``)
}

// LoadSandboxesForNode returns the records a node may act on at boot: the
// ones it owns plus the unowned ones. Unowned rows are safe to adopt, never
// safe to delete. An empty nodeID (unclustered) sees every row.
func (s *Store) LoadSandboxesForNode(ctx context.Context, nodeID string) ([]SandboxRecord, error) {
	if nodeID == "" {
		return s.LoadSandboxes(ctx)
	}
	return s.querySandboxes(ctx, `WHERE node_id = ? OR node_id = ''`, nodeID)
}

// SandboxByIdentityHash resolves a sandbox's own credential to its row: the
// control plane authenticates a pd_sb_ bearer with this, one indexed read per
// request and never cached, so destroy (which deletes the row) revokes on the
// next request on every replica. An empty hash never matches.
func (s *Store) SandboxByIdentityHash(ctx context.Context, hash string) (SandboxRecord, bool, error) {
	if hash == "" {
		return SandboxRecord{}, false, nil
	}
	return s.oneSandbox(ctx, `WHERE identity_hash = ? AND identity_hash <> '' LIMIT 1`, hash)
}

// StateLost marks a sandbox whose node was reaped: the machine is gone as far
// as the control plane can tell. The node overwrites it if it comes back and
// adopts the sandbox; a node that restarts drops its own unrestorable rows.
const StateLost = "lost"

// MarkNodeSandboxesLost flips every live row a reaped node owned to StateLost,
// so they stop counting against the org's concurrency and read as gone rather
// than as running forever. Returns how many rows changed.
func (s *Store) MarkNodeSandboxesLost(ctx context.Context, nodeID string) (int64, error) {
	if nodeID == "" {
		return 0, nil
	}
	// Paused rows stay paused: with a pause archive they resume on another
	// node (the control plane rehomes them on first use), and the state is
	// what says their archive is their latest. A rehome that finds no archive
	// marks the row lost then (MarkSandboxLost).
	res, err := s.exec(ctx,
		`UPDATE sandboxes SET state = ?, updated_at = ? WHERE node_id = ?
		   AND state NOT IN ('destroyed','stopped','error','lost','paused')`,
		StateLost, time.Now().Unix(), nodeID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// MarkSandboxLost marks one row lost: a paused sandbox of a reaped node that
// no live node could rehome.
func (s *Store) MarkSandboxLost(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `UPDATE sandboxes SET state = ?, updated_at = ? WHERE id = ? AND state = 'paused'`,
		StateLost, time.Now().Unix(), id)
	return err
}

// ReviveLost gives back a live state to this node's lost rows that the node
// still reports holding (id -> its state there). One read finds the lost rows,
// so a node with none costs a single indexed query per reconcile.
func (s *Store) ReviveLost(ctx context.Context, nodeID string, live map[string]string) (int, error) {
	if nodeID == "" || len(live) == 0 {
		return 0, nil
	}
	lost, err := collectRows(ctx, s, `SELECT id FROM sandboxes WHERE node_id = ? AND state = ?`,
		[]any{nodeID, StateLost}, scanInto(func(sc rowScanner) (string, error) {
			var id string
			return id, sc.Scan(&id)
		}))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range lost {
		state, ok := live[id]
		if !ok || state == "" || state == StateLost {
			continue
		}
		if _, err := s.exec(ctx, `UPDATE sandboxes SET state = ?, updated_at = ? WHERE id = ? AND state = ?`,
			state, time.Now().Unix(), id, StateLost); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// SandboxIDsOnTemplate returns the id of every stored sandbox on template, on
// any node and in any state: a node keeps a capture while one names it.
// ponytail: unindexed scan, run once per destroy of a capture's last loaded
// user; index template if the table grows past what that read can afford.
func (s *Store) SandboxIDsOnTemplate(ctx context.Context, template string) ([]string, error) {
	return collectRows(ctx, s, `SELECT id FROM sandboxes WHERE template = ?`,
		[]any{template}, scanInto(func(sc rowScanner) (string, error) {
			var id string
			return id, sc.Scan(&id)
		}))
}

// SandboxByID returns one live sandbox row, any org.
func (s *Store) SandboxByID(ctx context.Context, id string) (SandboxRecord, bool, error) {
	return s.oneSandbox(ctx, `WHERE id = ?`, id)
}

// DeleteSandbox removes a sandbox record, the custom domains pointed at it and
// its port forwards. The child rows are deleted here rather than by a foreign
// key: SQLite ignores ON DELETE CASCADE without PRAGMA foreign_keys, and a host
// or a public port left routing at a destroyed sandbox would keep answering for
// its next holder.
func (s *Store) DeleteSandbox(ctx context.Context, id string) error {
	if _, err := s.exec(ctx, `DELETE FROM custom_domains WHERE sandbox_id = ?`, id); err != nil {
		return err
	}
	if _, err := s.exec(ctx, `DELETE FROM port_forwards WHERE sandbox_id = ?`, id); err != nil {
		return err
	}
	_, err := s.exec(ctx, `DELETE FROM sandboxes WHERE id = ?`, id)
	return err
}

// ForgetSandbox deletes a sandbox and everything bound to it: chat links and
// messages, cron jobs, config, integrations and agent webhooks, then the row.
// Every destroy path must come here, not to DeleteSandbox alone, or the bound
// rows outlive it; an orphaned cron job on a private sandbox leaked its command
// text to the whole org. The agent mirror deliberately survives, bounded by
// retention. Per-table cleanup is best-effort; the sandbox row is deleted last.
func (s *Store) ForgetSandbox(ctx context.Context, id string) error {
	for _, forget := range []func(context.Context, string) error{
		s.DeleteChatLinksForSandbox,
		s.DeleteChatMessagesForSandbox,
		s.DeleteCronJobsForSandbox,
		s.DeleteSandboxConfig,
		s.DeleteSandboxIntegrations,
		s.DeleteAgentWebhooksForSandbox,
		s.deletePreviewGeneration,
	} {
		_ = forget(ctx, id)
	}
	return s.DeleteSandbox(ctx, id)
}

// SandboxesByOrg returns the live sandboxes for one org, id-ordered.
func (s *Store) SandboxesByOrg(ctx context.Context, orgID string) ([]SandboxRecord, error) {
	return s.querySandboxes(ctx, `WHERE org_id = ? ORDER BY id`, orgID)
}

// ActiveSandboxRow is the operator-facing subset of a live sandbox row. env
// is deliberately excluded; it may hold sealed customer secrets.
type ActiveSandboxRow struct {
	ID       string `json:"id"`
	Template string `json:"template"`
	OrgID    string `json:"org_id"`
	State    string `json:"state"`
	StartCmd string `json:"start_cmd,omitempty"`
	Source   string `json:"source,omitempty"`
	// CreatedBy and Visibility are the principal below the org; "" visibility
	// is a pre-migration row, which the org sees.
	CreatedBy  string `json:"created_by,omitempty"`
	Visibility string `json:"visibility,omitempty"`
	PublicPort int    `json:"public_port,omitempty"`
	VCPU       int    `json:"vcpu"`
	MemMiB     int    `json:"mem_mib"`
	CreatedAt  int64  `json:"created_at"`
	ExpiresAt  int64  `json:"expires_at"`
}

// activeSandboxCols is the column list, in scanActiveSandbox's order.
const activeSandboxCols = `id, template, org_id, state, start_cmd, source, created_by, visibility,
	public_port, vcpu, mem_mib, created_at, expires_at`

func scanActiveSandbox(sc rowScanner) (ActiveSandboxRow, error) {
	var r ActiveSandboxRow
	err := sc.Scan(&r.ID, &r.Template, &r.OrgID, &r.State, &r.StartCmd, &r.Source, &r.CreatedBy, &r.Visibility,
		&r.PublicPort, &r.VCPU, &r.MemMiB, &r.CreatedAt, &r.ExpiresAt)
	return r, err
}

// GetActiveSandboxRow returns one live sandbox row by id, cross-org, for
// operator-only paths. ok=false when the id is not in the table.
func (s *Store) GetActiveSandboxRow(ctx context.Context, id string) (ActiveSandboxRow, bool, error) {
	r, err := scanActiveSandbox(s.queryRow(ctx, `SELECT `+activeSandboxCols+` FROM sandboxes WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ActiveSandboxRow{}, false, nil
	}
	if err != nil {
		return ActiveSandboxRow{}, false, err
	}
	return r, true, nil
}

// ActiveSandboxRowsByOrg returns one org's live sandboxes as the env-free
// projection, newest first, so it works on a store with no secret key.
// Returns a non-nil empty slice when there are none.
func (s *Store) ActiveSandboxRowsByOrg(ctx context.Context, orgID string) ([]ActiveSandboxRow, error) {
	return collectRows(ctx, s,
		`SELECT `+activeSandboxCols+` FROM sandboxes WHERE org_id = ? ORDER BY created_at DESC`,
		[]any{orgID}, scanInto(scanActiveSandbox))
}
