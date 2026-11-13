package daemon

import (
	"context"
	"encoding/json"

	"github.com/pilldrops-oss/pilldrops/internal/auth/aat"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

func marshalEnv(env map[string]string) string {
	if len(env) == 0 {
		return "{}"
	}
	b, err := json.Marshal(env)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// marshalTags and unmarshalTags move the network tag list in and out of its
// column. Invalid input decodes to no tags, which is the closed default: a
// corrupt row must not put a sandbox on a network.
func marshalTags(tags []string) string {
	if len(tags) == 0 {
		return "[]"
	}
	b, err := json.Marshal(tags)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func unmarshalTags(s string) []string {
	if s == "" || s == "[]" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

func marshalPorts(ports []int) string {
	if len(ports) == 0 {
		return ""
	}
	b, err := json.Marshal(ports)
	if err != nil {
		return ""
	}
	return string(b)
}

func unmarshalPorts(s string) []int {
	if s == "" {
		return nil
	}
	var out []int
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

// unmarshalEnv returns an empty map on blank or invalid input.
func unmarshalEnv(s string) map[string]string {
	m := map[string]string{}
	if s != "" {
		_ = json.Unmarshal([]byte(s), &m)
	}
	return m
}

// StoreAdapter adapts *store.Store to the daemon's Persister interface.
type StoreAdapter struct {
	S *store.Store

	// NodeID is stamped on every save and scopes the boot load, so nodes
	// sharing a table only act on their own rows. Empty means unclustered.
	NodeID string

	// NodeClass (NODE_LABELS=class=...) is stamped on every metering row; empty is unclassified.
	NodeClass string
}

func (a StoreAdapter) SaveSandbox(ctx context.Context, rec PersistedSandbox) error {
	// Stamp ownership here, the one choke point every save site passes through.
	if rec.NodeID == "" {
		rec.NodeID = a.NodeID
	}
	return a.S.SaveSandbox(ctx, store.SandboxRecord{
		ID: rec.ID, Template: rec.Template, Name: rec.Name, Slug: rec.Slug, OrgID: rec.OrgID, State: rec.State,
		NodeID:   rec.NodeID,
		Snapshot: rec.Snapshot, StartCmd: rec.StartCmd, PublicPort: rec.PublicPort,
		VCPU: rec.VCPU, MemMiB: rec.MemMiB, StorageMB: rec.StorageMB,
		IdleTimeoutSecs:      rec.IdleTimeoutSecs,
		HibernateTimeoutSecs: rec.HibernateTimeoutSecs,
		BackupRetentionSecs:  rec.BackupRetentionSecs,
		Env:                  marshalEnv(rec.Env),
		IdentityHash:         rec.IdentityHash,
		Networks:             marshalTags(rec.Networks),
		CreatedBy:            rec.CreatedBy,
		Visibility:           rec.Visibility,
		CreatedAt:            rec.CreatedAt, ExpiresAt: rec.ExpiresAt, UpdatedAt: rec.UpdatedAt,
		PauseArchive: rec.PauseArchive,
		Image:        rec.Image, Ports: marshalPorts(rec.Ports),
	})
}

// DeleteSandbox forgets a sandbox and everything bound to it; see store.ForgetSandbox.
func (a StoreAdapter) DeleteSandbox(ctx context.Context, id string) error {
	return a.S.ForgetSandbox(ctx, id)
}

func (a StoreAdapter) RecordHistory(ctx context.Context, r SandboxHistory) error {
	return a.S.RecordHistory(ctx, store.HistoryRecord{
		ID: r.ID, OrgID: r.OrgID, Template: r.Template, Node: r.Node, ExitState: r.ExitState,
		CreatedAt: r.CreatedAt, DestroyedAt: r.DestroyedAt, DurationSecs: r.DurationSecs,
		VCPU: r.VCPU, MemMiB: r.MemMiB,
		VCPUSeconds: r.Usage.VCPUSeconds, RAMGBSeconds: r.Usage.RAMGBSeconds,
		Logs: r.Logs,
	})
}

func (a StoreAdapter) RecordEvent(ctx context.Context, orgID, sandboxID, event, template string, activeAfter int, at int64) error {
	return a.S.RecordEvent(ctx, store.EventRecord{
		OrgID: orgID, SandboxID: sandboxID, Event: event, Template: template,
		ActiveAfter: activeAfter, At: at,
	})
}

func (a StoreAdapter) RecordUsage(ctx context.Context, sandboxID, orgID string, u Usage) error {
	if err := a.S.RecordUsage(ctx, sandboxID, orgID, store.UsageDelta{
		VCPUSeconds: u.VCPUSeconds, RAMGBSeconds: u.RAMGBSeconds,
		StorageGBSeconds: u.StorageGBSeconds, RunSeconds: u.RunSeconds,
		NodeClass: a.NodeClass,
	}); err != nil {
		return err
	}
	// Not priced here: the node measures, the control plane's biller debits.
	return nil
}

// RecordUsageBatch satisfies the daemon's optional usageBatchWriter capability.
func (a StoreAdapter) RecordUsageBatch(ctx context.Context, recs []UsageRecord) error {
	entries := make([]store.UsageEntry, 0, len(recs))
	for _, r := range recs {
		entries = append(entries, store.UsageEntry{
			SandboxID: r.SandboxID, OrgID: r.OrgID,
			Delta: store.UsageDelta{
				VCPUSeconds: r.Usage.VCPUSeconds, RAMGBSeconds: r.Usage.RAMGBSeconds,
				StorageGBSeconds: r.Usage.StorageGBSeconds, RunSeconds: r.Usage.RunSeconds,
				NodeClass: a.NodeClass,
			},
		})
	}
	return a.S.RecordUsageBatch(ctx, entries)
}

// LoadSandboxes returns only the records this node may act on, so a node
// sharing a database does not adopt, meter or delete its peers' sandboxes.
func (a StoreAdapter) LoadSandboxes(ctx context.Context) ([]PersistedSandbox, error) {
	recs, err := a.S.LoadSandboxesForNode(ctx, a.NodeID)
	if err != nil {
		return nil, err
	}
	out := make([]PersistedSandbox, 0, len(recs))
	for _, r := range recs {
		out = append(out, persistedOf(r))
	}
	return out, nil
}

// OwnedElsewhere satisfies the daemon's ownerChecker: the row names another node.
func (a StoreAdapter) OwnedElsewhere(ctx context.Context, id string) (bool, error) {
	if a.NodeID == "" {
		return false, nil
	}
	r, ok, err := a.S.SandboxByID(ctx, id)
	if err != nil || !ok {
		return false, err
	}
	return r.NodeID != "" && r.NodeID != a.NodeID, nil
}

// SandboxRecord satisfies the daemon's recordGetter: one row, whichever node owns it.
func (a StoreAdapter) SandboxRecord(ctx context.Context, id string) (PersistedSandbox, bool, error) {
	r, ok, err := a.S.SandboxByID(ctx, id)
	if err != nil || !ok {
		return PersistedSandbox{}, ok, err
	}
	return persistedOf(r), true, nil
}

// SandboxIDsOnTemplate finds every stored record on a template.
func (a StoreAdapter) SandboxIDsOnTemplate(ctx context.Context, template string) ([]string, error) {
	return a.S.SandboxIDsOnTemplate(ctx, template)
}

func persistedOf(r store.SandboxRecord) PersistedSandbox {
	return PersistedSandbox{
		ID: r.ID, Template: r.Template, Name: r.Name, Slug: r.Slug, OrgID: r.OrgID, State: r.State,
		NodeID:   r.NodeID,
		Snapshot: r.Snapshot, StartCmd: r.StartCmd, PublicPort: r.PublicPort,
		VCPU: r.VCPU, MemMiB: r.MemMiB, StorageMB: r.StorageMB,
		IdleTimeoutSecs:      r.IdleTimeoutSecs,
		HibernateTimeoutSecs: r.HibernateTimeoutSecs,
		BackupRetentionSecs:  r.BackupRetentionSecs,
		Env:                  unmarshalEnv(r.Env),
		IdentityHash:         r.IdentityHash,
		Networks:             unmarshalTags(r.Networks),
		CreatedBy:            r.CreatedBy,
		Visibility:           r.Visibility,
		CreatedAt:            r.CreatedAt, ExpiresAt: r.ExpiresAt, UpdatedAt: r.UpdatedAt,
		PauseArchive: r.PauseArchive,
		Image:        r.Image, Ports: unmarshalPorts(r.Ports),
	}
}

// RevokeConsoleAgentTokens satisfies the daemon's optional agentTokenRevoker.
func (a StoreAdapter) RevokeConsoleAgentTokens(ctx context.Context, sandboxID string) (int, error) {
	ids, err := a.S.RevokeAgentTokensWithScope(ctx, sandboxID, aat.ScopeConsole)
	return len(ids), err
}

// RevokeAllAgentTokens satisfies the daemon's optional agentTokenRevoker.
func (a StoreAdapter) RevokeAllAgentTokens(ctx context.Context, sandboxID string) (int, error) {
	return a.S.RevokeAgentTokensForSandbox(ctx, sandboxID)
}

// Volume records, for the daemon's volumeStore.
func (a StoreAdapter) CreateVolume(ctx context.Context, v store.Volume) error {
	return a.S.CreateVolume(ctx, v)
}
func (a StoreAdapter) ListVolumes(ctx context.Context, org string) ([]store.Volume, error) {
	return a.S.ListVolumes(ctx, org)
}
func (a StoreAdapter) DeleteVolume(ctx context.Context, id string) error {
	return a.S.DeleteVolume(ctx, id)
}
func (a StoreAdapter) GetVolume(ctx context.Context, id string) (store.Volume, bool, error) {
	return a.S.GetVolume(ctx, id)
}
func (a StoreAdapter) AttachVolume(ctx context.Context, id, sandboxID string) error {
	return a.S.AttachVolume(ctx, id, sandboxID)
}
func (a StoreAdapter) DetachVolumes(ctx context.Context, sandboxID string) error {
	return a.S.DetachVolumes(ctx, sandboxID)
}
func (a StoreAdapter) VolumesAttachedTo(ctx context.Context, sandboxID string) ([]string, error) {
	return a.S.VolumesAttachedTo(ctx, sandboxID)
}
