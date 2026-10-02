package daemon

import (
	"context"
	"log/slog"
	"sync"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
)

// Config wires a Daemon to its runtime and policy.
type Config struct {
	Runtime rt.Runtime
	Store   Persister // optional; nil = no persistence
	Clock   Clock     // optional; nil = realClock
	// NodeID names this host in multi-node deploys; empty in single-node.
	NodeID      string
	IdleTimeout time.Duration
	// HibernateTimeout is the node-wide disk-pause timer; 0 = 4 h, <0 = never.
	// IdleTimeout is the tier above it: standby, frozen in RAM.
	HibernateTimeout time.Duration
	HardTTL          time.Duration
	// AbandonAfter destroys a persistent sandbox (no ExpiresAt) that has not
	// been resumed for this long. 0 = 30 days; <0 = never.
	AbandonAfter time.Duration
	DefaultVCPU  int
	DefaultMem   int
	// SnapshotDir defaults to <tmp>/sandboxd-snapshots and is created on New.
	SnapshotDir string
	// SnapshotArchive, if set, keeps a durable off-node copy of each pause snapshot.
	SnapshotArchive SnapshotArchive
	// Backups is the row index for sandbox backups and Blobs is the object
	// store their bytes go to. Both nil (the default) means this node cannot
	// take backups and says so with 501 rather than pretending to.
	Backups BackupStore
	Blobs   BlobStore
	// Logger is optional; nil disables daemon logging. Sandbox and org ids are
	// passed as attributes so the log can be filtered per machine.
	Logger *slog.Logger

	// PinTemplate holds a template's artifacts until the returned func is
	// called; Create pins before boot, Destroy releases. A func rather than the
	// cache itself because the daemon must not link the storage plane. Optional.
	PinTemplate func(template string) func()

	// IdentityAPIURL is the control-plane URL a guest calls as itself
	// (PILLDROPS_API_URL, else ORCHESTRATOR_URL). Empty disables sandbox
	// identity: nothing is minted or pushed and the row keeps no hash.
	IdentityAPIURL string
	// SandboxDomain is the preview wildcard domain handed to the guest as
	// PD_SANDBOX_DOMAIN; "" when the node has none.
	SandboxDomain string
}

// Persister is the store surface the daemon needs; *store.Store satisfies it via StoreAdapter.
type Persister interface {
	SaveSandbox(ctx context.Context, rec PersistedSandbox) error
	DeleteSandbox(ctx context.Context, id string) error
	RecordUsage(ctx context.Context, sandboxID, orgID string, u Usage) error
	LoadSandboxes(ctx context.Context) ([]PersistedSandbox, error)
	// RecordHistory retains a destroyed sandbox's metadata and logs.
	RecordHistory(ctx context.Context, rec SandboxHistory) error
	// RecordEvent appends a lifecycle event with the org's running count after it.
	RecordEvent(ctx context.Context, orgID, sandboxID, event, template string, activeAfter int, at int64) error
}

// SandboxHistory is the retained record of a destroyed sandbox. Times are unix seconds.
type SandboxHistory struct {
	ID, OrgID, Template, Node, ExitState string
	CreatedAt, DestroyedAt               int64
	DurationSecs                         int64
	VCPU, MemMiB                         int
	// Usage is the metered lifetime total; recorded, never priced here.
	Usage Usage
	Logs  string
}

// PersistedSandbox is the durable subset of a sandbox exchanged with the Persister. Times are unix seconds.
type PersistedSandbox struct {
	ID, Template, Name, Slug, OrgID, State, Snapshot, StartCmd string
	// NodeID is the owning node, or "" for unowned; restore only deletes owned records.
	NodeID                          string
	PublicPort                      int
	VCPU, MemMiB, StorageMB         int
	IdleTimeoutSecs                 int // per-sandbox standby timer; 0=default, <0=never
	HibernateTimeoutSecs            int // per-sandbox disk-pause timer; 0=default, <0=never
	BackupRetentionSecs             int // per-sandbox backup retention; 0=BACKUPS_RETAIN_DAYS
	Env                             map[string]string
	IdentityHash                    string   // sha256 of the sandbox's pd_sb_ token; "" = none
	Networks                        []string // opt-in network tags; empty = joins none
	CreatedBy                       string   // API key that created or forked it; "" = legacy row
	Visibility                      string   // "org" (default, also for "") or "private"
	CreatedAt, ExpiresAt, UpdatedAt int64
	// PauseArchive is the id of the archived pause this paused record's state
	// is, "" for none; see store.SandboxRecord.PauseArchive.
	PauseArchive string
}

// Sandbox is the daemon's in-memory view of one sandbox. Fields are guarded by
// Daemon.mu; pause/resume/destroy are also serialized per sandbox by opMu.
type Sandbox struct {
	ID       string
	Template string
	// Name is an optional human label.
	Name string
	// Slug is the optional DNS label the sandbox is hosted under; the store's
	// unique index is the arbiter, see SetSlug.
	Slug     string
	OrgID    string
	State    State
	Snapshot rt.SnapshotRef
	// pauseArchive is the id of the archived pause this paused sandbox's
	// snapshot is, "" when none is (recordOf drops it outside paused).
	pauseArchive string
	CreatedAt    time.Time
	ExpiresAt    time.Time
	LastActiveAt time.Time
	// PublicPort is the app port declared via StartServer; 0 means the preview proxy imposes no restriction.
	PublicPort int
	// Ports are the request-declared ports published at boot on a single-port
	// runtime. In-memory only: a restart falls back to the catalogue's ports.
	Ports []int
	// BootType and BootMs are how this sandbox came up in this process; see
	// SandboxInfo. Not persisted: a sandbox adopted after a restart did not boot.
	BootType string
	BootMs   int64
	// StartCmd and Env are replayed on resume so a woken sandbox serves its app again.
	StartCmd string
	Env      map[string]string
	// VCPU, MemMiB, StorageMB are replayed on resume and used to meter usage.
	VCPU      int
	MemMiB    int
	StorageMB int
	// BackupRetention is how long this sandbox's backups are kept; 0 means the
	// deployment default decides. Persisted on the sandbox row: the schedule
	// job does not carry the number, and a restart used to revert every
	// sandbox to BACKUPS_RETAIN_DAYS without saying so.
	BackupRetention time.Duration
	// IdleTimeout: 0 = daemon default; <0 = never. Standby, not disk.
	IdleTimeout time.Duration
	// HibernateTimeout: 0 = daemon default; <0 = never. The disk tier.
	HibernateTimeout time.Duration
	// IdentityHash is the sha256 of the sandbox's own pd_sb_ token. The token
	// itself is never held here: it went to the runtime for delivery and then
	// exists only in the guest.
	IdentityHash string
	// Networks are the opt-in network tags this sandbox joined; empty is the
	// default and reaches no peer. PrivateIP is the address peers reach it on,
	// "" when it is on no network; the runtime reports it, the daemon does not
	// derive it.
	Networks  []string
	PrivateIP string
	// CreatedBy is the API key that created or forked this sandbox ("" for a
	// row that predates visibility); Visibility is "org" or "private" ("" reads
	// as "org"). The daemon carries both so the HTTP layer can judge a paused
	// sandbox from the record without waking it.
	CreatedBy  string
	Visibility string

	// diskMB and diskAt cache the last storage measurement; kept across pause.
	diskMB int
	diskAt time.Time

	opMu  sync.Mutex // serializes pause/resume/destroy for this sandbox
	inst  rt.Instance
	meter *meter
	// unpin releases the template pin. Held until destroy, not pause, since a
	// paused sandbox resumes from the same rootfs.
	unpin func()
	// oomSeen is the last cumulative OOM-kill count observed.
	oomSeen int
	// serverUp is the last observed liveness of the registered start server.
	serverUp bool
	// probeFails counts the scans in a row whose exec into probeInst failed.
	probeFails int
	probeInst  rt.Instance
}

// SandboxInfo is an immutable snapshot of a sandbox's public state. The HTTP
// layer mirrors State to "status" for E2B/CLI compatibility.
type SandboxInfo struct {
	ID           string         `json:"id"`
	Template     string         `json:"template"`
	Name         string         `json:"name,omitempty"`
	Slug         string         `json:"slug,omitempty"`
	OrgID        string         `json:"org_id"`
	State        State          `json:"state"`
	Snapshot     rt.SnapshotRef `json:"-"`
	CreatedAt    time.Time      `json:"created_at"`
	ExpiresAt    time.Time      `json:"expires_at"`
	LastActiveAt time.Time      `json:"last_active_at"`
	PublicPort   int            `json:"public_port,omitempty"`
	VCPU         int            `json:"vcpu,omitempty"`
	MemMiB       int            `json:"mem_mib,omitempty"`
	StorageMB    int            `json:"storage_mb,omitempty"`
	// IdleTimeout and HibernateTimeout: 0 = daemon default; <0 = never. The HTTP
	// layer renders both in seconds.
	IdleTimeout      time.Duration `json:"-"`
	HibernateTimeout time.Duration `json:"-"`
	// Networks are the sandbox's network tags; PrivateIP is where peers reach it.
	Networks  []string `json:"networks,omitempty"`
	PrivateIP string   `json:"private_ip,omitempty"`
	// CreatedBy names the API key that made this sandbox; Visibility is "org"
	// or "private". Both are rendered on every sandbox response.
	CreatedBy  string `json:"created_by"`
	Visibility string `json:"visibility"`
	// StartCmd is excluded from JSON: it can embed tokens interpolated at deploy time.
	StartCmd string `json:"-"`
	// BootType is "snapshot" when this sandbox woke out of a memory image with
	// its processes already running, and "cold" when it booted a kernel. BootMs
	// is how long that took. Both are absent on a sandbox restored from the
	// store after a daemon restart, which did not boot in this process.
	BootType string `json:"boot_type,omitempty"`
	BootMs   int64  `json:"boot_ms,omitempty"`
}

// info copies the public fields; the caller must hold d.mu.
func (sb *Sandbox) info() SandboxInfo {
	return SandboxInfo{
		ID: sb.ID, Template: sb.Template, Name: sb.Name, Slug: sb.Slug, OrgID: sb.OrgID, State: sb.State,
		Snapshot: sb.Snapshot, CreatedAt: sb.CreatedAt, ExpiresAt: sb.ExpiresAt,
		LastActiveAt: sb.LastActiveAt, PublicPort: sb.PublicPort,
		VCPU: sb.VCPU, MemMiB: sb.MemMiB, StorageMB: sb.StorageMB,
		IdleTimeout: sb.IdleTimeout, HibernateTimeout: sb.HibernateTimeout,
		Networks: sb.Networks, PrivateIP: sb.PrivateIP,
		// Rendered, never stored, as "org" when unset: a legacy row and a
		// deliberately org-visible one are the same thing to every caller.
		CreatedBy: sb.CreatedBy, Visibility: visibilityOrDefault(sb.Visibility),
		StartCmd: sb.StartCmd,
		BootType: sb.BootType, BootMs: sb.BootMs,
	}
}

// recordOf builds a durable record; the caller must hold d.mu.
func (d *Daemon) recordOf(sb *Sandbox) PersistedSandbox {
	return PersistedSandbox{
		ID: sb.ID, Template: sb.Template, Name: sb.Name, Slug: sb.Slug, OrgID: sb.OrgID, State: string(sb.State),
		Snapshot: sb.Snapshot.Path, StartCmd: sb.StartCmd, PublicPort: sb.PublicPort,
		VCPU: sb.VCPU, MemMiB: sb.MemMiB, StorageMB: sb.StorageMB,
		IdleTimeoutSecs:      int(sb.IdleTimeout / time.Second),
		HibernateTimeoutSecs: int(sb.HibernateTimeout / time.Second),
		BackupRetentionSecs:  int(sb.BackupRetention / time.Second),
		Env:                  sb.Env,
		IdentityHash:         sb.IdentityHash,
		Networks:             sb.Networks,
		CreatedBy:            sb.CreatedBy,
		Visibility:           sb.Visibility,
		CreatedAt:            sb.CreatedAt.Unix(),
		ExpiresAt:            unixOrZero(sb.ExpiresAt), UpdatedAt: d.clock.Now().Unix(),
		// Only a paused sandbox is its archive: a running one has moved on, so
		// every save of it clears the id, and a rehome then refuses the archive.
		PauseArchive: pauseArchiveOf(sb),
	}
}

func pauseArchiveOf(sb *Sandbox) string {
	if sb.State != StatePaused {
		return ""
	}
	return sb.pauseArchive
}

// visibilityOrDefault renders an unset visibility as "org", which is what
// every sandbox was before the column existed.
func visibilityOrDefault(v string) string {
	if v == "" {
		return sandbox.VisibilityOrg
	}
	return v
}

// visibilityOrPrivate is the fork default: a child nobody asked to share is
// private to whoever forked it.
func visibilityOrPrivate(v string) string {
	if v == "" {
		return sandbox.VisibilityPrivate
	}
	return v
}

// unixOrZero maps the zero time to 0; time.Time{}.Unix() would read back as long expired.
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// timeOrZero is the inverse of unixOrZero: 0 means no expiry, not 1970.
func timeOrZero(sec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

// lastActive recovers a restored sandbox's idle clock from its last persisted
// write, falling back to now. Never returns a future time, which would stall abandonment.
func lastActive(updatedAt int64, now time.Time) time.Time {
	if updatedAt <= 0 {
		return now
	}
	t := time.Unix(updatedAt, 0)
	if t.After(now) {
		return now
	}
	return t
}
