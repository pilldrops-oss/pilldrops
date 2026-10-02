// Package daemon owns sandbox lifecycle: the state machine, the timeout
// reconciler, and usage metering. It drives a runtime.Runtime and optionally
// persists to a store.
package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/observability"
	"go.opentelemetry.io/otel/attribute"

	"github.com/pilldrops-oss/pilldrops/internal/egress"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// CreateSpec is a create request.
type CreateSpec struct {
	Template string
	// Name is an optional human label.
	Name    string
	OrgID   string
	Timeout time.Duration // hard TTL override; 0 = Config.HardTTL
	// IdleTimeout overrides the idle standby timer: 0 = daemon default; <0 = never.
	IdleTimeout time.Duration
	// HibernateTimeout overrides the disk-pause timer: 0 = daemon default; <0 = never.
	HibernateTimeout time.Duration
	// Resources; 0 falls back to Config.DefaultVCPU/DefaultMem and no disk cap.
	VCPU      int
	MemMiB    int
	StorageMB int
	// Image, if set, replaces the base sandbox image with its own entrypoint, Env and Ports.
	Image string
	Ports []int
	Env   []string // "NAME=VALUE"
	// PreviewBase is the URL prefix under which /<id>/<port>/ reaches the
	// sandbox (the API's path-form preview root). Non-empty sets PD_PREVIEW_BASE
	// in the guest env; PD_SANDBOX_ID is always set.
	PreviewBase string
	// Network is the egress policy, already resolved by internal/sandbox; nil is
	// unrestricted. A runtime that cannot enforce it refuses the boot.
	Network *egress.Rules
	// Networks are the opt-in network tags the sandbox joins; empty is the
	// default and reaches no peer. A runtime without TagNetworks refuses a
	// non-empty list rather than booting a sandbox that is on no network it
	// was told it would be on.
	Networks []string
	// Snapshot names a published named snapshot to wake from instead of booting
	// the template. Template still carries the snapshot's own template id, which
	// is what the node fetched and what SnapshotDir holds it under.
	Snapshot string
	// ID, when set, is the id this sandbox takes instead of a new one: the
	// control plane moving a sandbox between nodes. Its stored record is the
	// sandbox's durable config (name, slug, env, owner, timers), so the new
	// instance is built from that record and the id keeps meaning the same
	// sandbox. Set only for an operator caller; see httpapi.
	ID string
	// CreatedBy is the id of the API key that asked for this sandbox, and
	// Visibility is "org" (default) or "private". The daemon stores both and
	// decides nothing with them; the HTTP layer enforces the visibility rule.
	CreatedBy  string
	Visibility string
}

// guestEnv is the create env plus the platform's own PD_* entries, appended
// last so a caller-supplied PD_SANDBOX_ID cannot override them (later wins in
// both the Docker -e list and the Firecracker exec prefix).
func guestEnv(spec CreateSpec, id string) []string {
	env := append(append([]string(nil), spec.Env...), "PD_SANDBOX_ID="+id)
	if spec.PreviewBase != "" {
		env = append(env, "PD_PREVIEW_BASE="+strings.TrimSuffix(spec.PreviewBase, "/")+"/"+id)
	}
	return env
}

// Daemon is the lifecycle owner.
type Daemon struct {
	cfg     Config
	clock   Clock
	mu      sync.RWMutex
	boxes   map[string]*Sandbox
	pending map[string]int // per-org in-flight create reservations
	// rehoming holds one channel per sandbox a rehome is running for, closed
	// when it ends; guarded by mu.
	rehoming map[string]chan struct{}
	stop     chan struct{}
	stopOnce sync.Once
	// revives tracks restore's background casualty revives; reviveStop, closed
	// under mu, cancels them, and no revive is added once it is closed.
	// gone names the sandboxes removed while a restore is unfinished, so a
	// retry pass reading rows loaded before the removal does not bring one
	// back. Guarded by mu; nil once a restore completes.
	gone           map[string]bool
	revives        sync.WaitGroup
	reviveStop     chan struct{}
	reviveStopOnce sync.Once
	// bgCancel cancels the context shared by the reconciler and scanners.
	bgCancel context.CancelFunc
	// lastUsageFlush is guarded by mu.
	lastUsageFlush time.Time

	metrics *Metrics

	// liveStats single-flights and briefly caches the per-sandbox resource sample,
	// which costs ~1.7s per docker stats call.
	liveStats sampleGroup[rt.LiveStats]
	// diskSample single-flights the storage measurement.
	diskSample sampleGroup[int]
	// isolationOK is false only when the enforce-mode isolation preflight failed.
	// Checked in Create and Resume rather than the HTTP handlers because the
	// preview proxy and agent paths boot sandboxes without going through them.
	isolationOK atomic.Bool
}

// ErrIsolationPreflight is returned by Create and Resume when the node's
// isolation preflight failed under SANDBOXD_PREFLIGHT_ENFORCE.
var ErrIsolationPreflight = errors.New("node isolation preflight failed — refusing to boot sandboxes")

// ErrEgressUnsupported is a create carrying an egress policy on a node whose
// runtime cannot enforce one. Refused rather than ignored: a sandbox that reads
// as confined and reaches everything is worse than a create that fails.
var ErrEgressUnsupported = errors.New("this node's runtime cannot enforce a per-sandbox egress policy " +
	"(it needs the firecracker runtime with FC_NETNS=1); create the sandbox without one or place it on a node that can")

// ErrEgressDomainsUnsupported is a create naming a DOMAIN on a node with no
// name layer. Same reason as ErrEgressUnsupported and a narrower cause: this
// node enforces addresses and CIDRs in its kernel chain, and a domain needs
// something that reads the hostname off the wire.
var ErrEgressDomainsUnsupported = errors.New("this node cannot enforce domain egress rules: they need the " +
	"name layer that reads TLS SNI and the HTTP Host header, which runs only on the firecracker runtime " +
	"with FC_NETNS=1. Address and CIDR rules are enforced and work here")

// SetIsolationOK records whether the isolation preflight passed; false makes Create and Resume refuse.
func (d *Daemon) SetIsolationOK(ok bool) { d.isolationOK.Store(ok) }

// New builds a Daemon. Call Start to run the reconciler loop.
func New(cfg Config) *Daemon {
	if cfg.Clock == nil {
		cfg.Clock = realClock{}
	}
	// IdleTimeout now freezes a sandbox in RAM rather than writing it to disk,
	// and defaults OFF: a node opts into holding idle RAM. HibernateTimeout is
	// the timer that was 5 minutes, moved to 4 hours and to the disk tier.
	if cfg.HibernateTimeout == 0 {
		cfg.HibernateTimeout = 4 * time.Hour
	}
	if cfg.HardTTL == 0 {
		cfg.HardTTL = 24 * time.Hour
	}
	// Default on so an unconfigured deploy cannot leak persistent sandboxes forever; <0 opts out.
	if cfg.AbandonAfter == 0 {
		cfg.AbandonAfter = 30 * 24 * time.Hour
	}
	if cfg.DefaultVCPU == 0 {
		cfg.DefaultVCPU = 1
	}
	if cfg.DefaultMem == 0 {
		cfg.DefaultMem = 512
	}
	if cfg.SnapshotDir == "" {
		cfg.SnapshotDir = filepath.Join(os.TempDir(), "sandboxd-snapshots")
	}
	_ = os.MkdirAll(cfg.SnapshotDir, 0o755)
	d := &Daemon{
		cfg: cfg, clock: cfg.Clock,
		boxes: make(map[string]*Sandbox), pending: make(map[string]int), rehoming: make(map[string]chan struct{}), stop: make(chan struct{}), reviveStop: make(chan struct{}),
		metrics: newDaemonMetrics(),
	}
	// Anchor here so the first usage flush lands one full interval in, not on the first tick.
	d.lastUsageFlush = cfg.Clock.Now()
	d.isolationOK.Store(true)
	return d
}

// newSandboxID returns sb-<unixMillis>-<hex>. Only [a-z0-9-] so the id can be
// a DNS label in preview hostnames; older sb_<ms>_<hex> ids stay valid as opaque strings.
// ErrBadMoveID refuses a create that names an id it may not take.
var ErrBadMoveID = errors.New("cannot create under that id")

// recordGetter is the optional store lookup a moved-in create reads its
// config from.
type recordGetter interface {
	SandboxRecord(ctx context.Context, id string) (PersistedSandbox, bool, error)
}

// validSandboxID matches what newSandboxID mints.
func validSandboxID(id string) bool {
	ts, rnd, ok := strings.Cut(strings.TrimPrefix(id, "sb-"), "-")
	if !ok || !strings.HasPrefix(id, "sb-") || len(rnd) != 8 || ts == "" {
		return false
	}
	for _, c := range ts {
		if c < '0' || c > '9' {
			return false
		}
	}
	_, err := hex.DecodeString(rnd)
	return err == nil
}

func newSandboxID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("sandboxd: crypto/rand failed: %v", err))
	}
	return fmt.Sprintf("sb-%d-%s", time.Now().UnixMilli(), hex.EncodeToString(b))
}

// snapshotPath returns the absolute snapshot location for a sandbox.
func (d *Daemon) snapshotPath(id string) string {
	return filepath.Join(d.cfg.SnapshotDir, id+".snap")
}

// log returns the configured logger, or a discarding one.
func (d *Daemon) log() *slog.Logger {
	if d.cfg.Logger == nil {
		return slog.New(discardHandler{})
	}
	return d.cfg.Logger
}

type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (discardHandler) WithAttrs([]slog.Attr) slog.Handler        { return discardHandler{} }
func (discardHandler) WithGroup(string) slog.Handler             { return discardHandler{} }

// Create boots a new running sandbox.
func (d *Daemon) Create(ctx context.Context, spec CreateSpec) (info SandboxInfo, err error) {
	ctx, span := observability.StartInternal(ctx, "daemon.Create",
		attribute.String("template", spec.Template), attribute.String("org_id", spec.OrgID))
	defer func() { observability.End(span, err) }()

	if !d.isolationOK.Load() {
		return SandboxInfo{}, ErrIsolationPreflight
	}
	// Checked here, not only in the HTTP handler: every boot path passes through
	// Create, and a runtime that silently ignored the policy would hand back a
	// sandbox that reads as confined and reaches everything.
	if spec.Network != nil && (d.cfg.Runtime == nil || !d.cfg.Runtime.Capabilities().EgressPolicy) {
		return SandboxInfo{}, ErrEgressUnsupported
	}
	// A domain entry needs the name layer, which is a strictly narrower
	// capability than "enforces an egress policy": address and CIDR rules are
	// decided in the kernel chain and need nothing else. Refused, never
	// accepted and left unevaluated.
	if spec.Network.HasDomains() && (d.cfg.Runtime == nil || !d.cfg.Runtime.Capabilities().EgressDomains) {
		return SandboxInfo{}, ErrEgressDomainsUnsupported
	}
	// Same rule, same reason: refused here rather than only in the handler,
	// because every boot path passes through Create.
	if len(spec.Networks) > 0 && !d.tagNetworksOK() {
		return SandboxInfo{}, ErrTagNetworksUnsupported
	}
	// Validate here, not only in the HTTP handler: every boot path passes through
	// Create, and the name is joined into host paths downstream. Must run before
	// PinTemplate, which also takes the raw name.
	if err := rt.ValidateTemplateName(spec.Template); err != nil {
		return SandboxInfo{}, err
	}
	if spec.OrgID == "" {
		spec.OrgID = "default"
	}
	vcpu := spec.VCPU
	if vcpu <= 0 {
		vcpu = d.cfg.DefaultVCPU
	}
	memMiB := spec.MemMiB
	if memMiB <= 0 {
		memMiB = d.cfg.DefaultMem
	}
	id := newSandboxID()
	var moved *PersistedSandbox
	if spec.ID != "" {
		if !validSandboxID(spec.ID) {
			return SandboxInfo{}, fmt.Errorf("%w: %q is not a sandbox id", ErrBadMoveID, spec.ID)
		}
		d.mu.RLock()
		_, exists := d.boxes[spec.ID]
		d.mu.RUnlock()
		if exists {
			return SandboxInfo{}, fmt.Errorf("%w: %s already runs on this node", ErrBadMoveID, spec.ID)
		}
		if g, ok := d.cfg.Store.(recordGetter); ok {
			rec, found, err := g.SandboxRecord(ctx, spec.ID)
			if err != nil {
				return SandboxInfo{}, err
			}
			// Another org's id is not this caller's to take over.
			if found && rec.OrgID != spec.OrgID {
				return SandboxInfo{}, fmt.Errorf("%w: %s belongs to another org", ErrBadMoveID, spec.ID)
			}
			// A move always has the sandbox's row to carry. None means it was
			// destroyed while the move ran, and creating here would bring it
			// back with none of its config under an id its owner deleted.
			if !found {
				return SandboxInfo{}, fmt.Errorf("%w: %s has no record (destroyed during the move?)", ErrBadMoveID, spec.ID)
			}
			moved = &rec
		}
		id = spec.ID
	}
	// Pin the template's artifacts before boot and hold until destroy; cache
	// eviction cannot see which files a running VM has open.
	unpin := func() {}
	if d.cfg.PinTemplate != nil {
		if rel := d.cfg.PinTemplate(spec.Template); rel != nil {
			unpin = rel
		}
	}
	// The raw token lives in this BootSpec for the length of the push and nowhere else.
	ident, identHash := d.identityFor(id)
	bootSpec := rt.BootSpec{
		SandboxID: id,
		Template: rt.Template{
			Name: spec.Template, Image: spec.Image, Ports: spec.Ports, Env: guestEnv(spec, id),
		},
		VCPU:      vcpu,
		MemMiB:    memMiB,
		StorageMB: spec.StorageMB,
		Egress:    spec.Network,
		Identity:  ident,
	}
	bootCtx, bootSpan := observability.StartInternal(ctx, "runtime.Boot",
		attribute.String("sandbox_id", id), attribute.String("template", spec.Template),
		attribute.String("snapshot", spec.Snapshot))
	t0 := d.clock.Now()
	var inst rt.Instance
	bootType := "cold"
	if spec.Snapshot != "" {
		// Gated on the capability, not on the type assertion. Firecracker
		// always implements the interface and still cannot serve this without
		// per-sandbox namespaces; the capability is what the control plane
		// schedules on, so it has to be what the node refuses on, or a create
		// is placed here and fails several layers down with a worse message.
		booter, ok := d.cfg.Runtime.(rt.SnapshotBooter)
		if ok && !d.cfg.Runtime.Capabilities().NamedSnapshot {
			ok = false
		}
		if !ok {
			observability.End(bootSpan, rt.ErrUnsupported)
			unpin()
			return SandboxInfo{}, fmt.Errorf("%w: the %s runtime cannot boot from a named snapshot",
				rt.ErrUnsupported, d.RuntimeName())
		}
		// Path is the directory the fetch materialised, which is the template
		// directory: a snapshot rides an ordinary template id all the way down.
		// Name is for engines that key on a string rather than a directory.
		var kind string
		inst, kind, err = booter.BootFromSnapshot(bootCtx,
			rt.SnapshotRef{Path: d.snapshotDir(spec.Template), Name: spec.Template}, bootSpec)
		if err == nil {
			bootType = bootTypeForKind(kind)
		}
	} else {
		inst, err = d.cfg.Runtime.Boot(bootCtx, bootSpec)
		// A plain boot may still have been a golden restore; ask the runtime.
		if bt, ok := inst.(rt.BootTyper); ok && err == nil && bt.BootType() != "" {
			bootType = bt.BootType()
		}
	}
	bootMs := d.clock.Now().Sub(t0).Milliseconds()
	observability.End(bootSpan, err)
	span.SetAttributes(attribute.String("sandbox_id", id))
	if err != nil {
		unpin()
		return SandboxInfo{}, fmt.Errorf("boot: %w", err)
	}
	now := d.clock.Now()
	ttl := spec.Timeout
	if ttl == 0 {
		ttl = d.cfg.HardTTL
	}
	// Negative timeout means no hard TTL: zero ExpiresAt, which the reaper skips.
	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = now.Add(ttl)
	}
	if moved != nil {
		// A moved sandbox is the stored one, now running here from the
		// template it was captured into.
		sb := d.sandboxFromRecord(*moved, now)
		sb.Template, sb.State, sb.LastActiveAt = spec.Template, StateRunning, now
		sb.IdentityHash, sb.BootType, sb.BootMs = identHash, bootType, bootMs
		sb.inst, sb.unpin = inst, unpin
		// As on resume: relaunch the app unless the runtime brought memory back
		// with it, or two copies would fight over the port.
		if sb.StartCmd != "" && !d.cfg.Runtime.Capabilities().MemoryRestore {
			if err := inst.StartServer(ctx, sb.StartCmd, sb.Env); err != nil {
				d.log().Warn("move: app restart failed", "sandbox_id", id, "error", err)
			}
		}
		return d.finishCreate(ctx, sb, spec)
	}
	sb := &Sandbox{
		ID: id, Template: spec.Template, Name: spec.Name, OrgID: spec.OrgID, State: StateRunning,
		CreatedAt: now, ExpiresAt: expiresAt, LastActiveAt: now,
		VCPU: vcpu, MemMiB: memMiB, StorageMB: spec.StorageMB,
		IdleTimeout:      spec.IdleTimeout,
		HibernateTimeout: spec.HibernateTimeout,
		Ports:            spec.Ports,
		IdentityHash:     identHash,
		Networks:         spec.Networks,
		CreatedBy:        spec.CreatedBy,
		Visibility:       spec.Visibility,
		BootType:         bootType,
		BootMs:           bootMs,
		inst:             inst,
		meter:            newMeter(vcpu, memMiB, spec.StorageMB, now),
		unpin:            unpin,
	}
	return d.finishCreate(ctx, sb, spec)
}

// finishCreate registers a booted sandbox and persists it.
func (d *Daemon) finishCreate(ctx context.Context, sb *Sandbox, spec CreateSpec) (info SandboxInfo, err error) {
	id := sb.ID
	d.mu.Lock()
	d.boxes[id] = sb
	d.mu.Unlock()
	// After the sandbox is in d.boxes and before it is handed back: a create
	// that asked for a network and got a sandbox on none is a failed create.
	if len(spec.Networks) > 0 {
		if err := d.pushNetworks(ctx, id); err != nil {
			_ = d.Destroy(context.WithoutCancel(ctx), id)
			return SandboxInfo{}, fmt.Errorf("join networks: %w", err)
		}
	}
	d.mu.Lock()
	info = sb.info()
	rec := d.recordOf(sb)
	d.mu.Unlock()
	d.persistRec(ctx, rec)
	d.recordEvent(ctx, spec.OrgID, id, "created", spec.Template)
	d.log().Info("sandbox created", "sandbox_id", id, "template", spec.Template, "org_id", spec.OrgID)
	return info, nil
}

// UsageSnapshot is a live view of one sandbox's accrued resource usage.
type UsageSnapshot struct {
	State         State   `json:"state"`
	VCPU          int     `json:"vcpu"`
	MemMiB        int     `json:"mem_mib"`
	VCPUSeconds   float64 `json:"vcpu_seconds"`
	RAMGBSeconds  float64 `json:"ram_gb_seconds"`
	UptimeSeconds float64 `json:"uptime_seconds"`
}

// Usage returns a live usage snapshot without advancing the sandbox's meter.
func (d *Daemon) Usage(id string) (UsageSnapshot, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	sb, ok := d.boxes[id]
	if !ok {
		return UsageSnapshot{}, false
	}
	now := d.clock.Now()
	pk := sb.meter.peek(now)
	vcpu, ram := pk.VCPUSeconds, pk.RAMGBSeconds
	return UsageSnapshot{
		State:         sb.State,
		VCPU:          sb.meter.vcpu,
		MemMiB:        sb.meter.memMiB,
		VCPUSeconds:   vcpu,
		RAMGBSeconds:  ram,
		UptimeSeconds: now.Sub(sb.CreatedAt).Seconds(),
	}, true
}

// Get returns an immutable snapshot of a sandbox by id.
func (d *Daemon) Get(id string) (SandboxInfo, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	sb, ok := d.boxes[id]
	if !ok {
		return SandboxInfo{}, false
	}
	return sb.info(), true
}

// List returns immutable snapshots for an org (all if orgID == "").
func (d *Daemon) List(orgID string) []SandboxInfo {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]SandboxInfo, 0, len(d.boxes))
	for _, sb := range d.boxes {
		if orgID == "" || sb.OrgID == orgID {
			out = append(out, sb.info())
		}
	}
	return out
}

// ReserveConcurrency reserves one per-org slot against max, counting existing
// sandboxes plus in-flight reservations under one lock; the caller must defer
// release. max <= 0 means no cap. Node-local only.
func (d *Daemon) ReserveConcurrency(orgID string, max int) (release func(), ok bool) {
	if max <= 0 {
		return func() {}, true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	current := 0
	for _, sb := range d.boxes {
		if sb.OrgID == orgID {
			current++
		}
	}
	if current+d.pending[orgID] >= max {
		return nil, false
	}
	d.pending[orgID]++
	var once sync.Once
	return func() {
		once.Do(func() {
			d.mu.Lock()
			defer d.mu.Unlock()
			if d.pending[orgID] > 0 {
				d.pending[orgID]--
			}
			if d.pending[orgID] == 0 {
				delete(d.pending, orgID)
			}
		})
	}, true
}

// agentTokenRevoker is an optional Persister capability. Revocation lives in the
// daemon because most pauses come from the reconciler, not an HTTP request.
type agentTokenRevoker interface {
	RevokeConsoleAgentTokens(ctx context.Context, sandboxID string) (int, error)
	RevokeAllAgentTokens(ctx context.Context, sandboxID string) (int, error)
}

// revokeConsoleTokens is best-effort: failing the pause would leave a machine running and billing.
func (d *Daemon) revokeConsoleTokens(ctx context.Context, id string) {
	r, ok := d.cfg.Store.(agentTokenRevoker)
	if !ok {
		return
	}
	n, err := r.RevokeConsoleAgentTokens(ctx, id)
	if err != nil {
		d.log().Warn("revoke console tokens failed", "sandbox_id", id, "error", err)
		return
	}
	if n > 0 {
		d.log().Info("revoked console tokens on pause", "sandbox_id", id, "count", n)
	}
}

// revokeAgentTokens revokes every token for a destroyed sandbox; a live token
// for a dead id is a credential for whatever next answers to that id.
func (d *Daemon) revokeAgentTokens(ctx context.Context, id string) {
	r, ok := d.cfg.Store.(agentTokenRevoker)
	if !ok {
		return
	}
	if _, err := r.RevokeAllAgentTokens(ctx, id); err != nil {
		d.log().Warn("revoke agent tokens failed", "sandbox_id", id, "error", err)
	}
}

// persistRec writes a durable record if a store is configured.
func (d *Daemon) persistRec(ctx context.Context, r PersistedSandbox) {
	if d.cfg.Store == nil {
		return
	}
	if err := d.cfg.Store.SaveSandbox(ctx, r); err != nil {
		d.log().Error("persist sandbox failed", "sandbox_id", r.ID, "error", err)
	}
}

// flushUsage records a non-zero metered delta if a store is configured.
func (d *Daemon) flushUsage(ctx context.Context, id, orgID string, u Usage) {
	if d.cfg.Store == nil || u.IsZero() {
		return
	}
	if err := d.cfg.Store.RecordUsage(ctx, id, orgID, u); err != nil {
		d.log().Error("record usage failed", "sandbox_id", id, "org_id", orgID, "error", err)
	}
}

// snapshotDir is where the template fetcher materialised a template's files.
// A named snapshot rides an ordinary template id, so its capture sits in that
// template's directory beside the rootfs the overlay is a delta against.
func (d *Daemon) snapshotDir(templateID string) string {
	return filepath.Join(templates.Dir(), templateID)
}

// bootTypeForKind translates what the engine says it restored into what the API
// calls it. A "memory" capture wakes with its processes running, which the API
// has always called a snapshot boot; a "disk" one is a filesystem and a fresh
// kernel, which is a cold boot however it was obtained.
func bootTypeForKind(kind string) string {
	if kind == "memory" {
		return "snapshot"
	}
	return "cold"
}
