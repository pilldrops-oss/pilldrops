package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// UsageSummary is aggregated metering for an org.
type UsageSummary struct {
	VCPUSeconds  float64
	RAMGBSeconds float64
	// StorageGBSeconds accrues while paused too; a paused sandbox still holds its disk.
	StorageGBSeconds float64
	// RunSeconds is wall-clock running time, the unit plan entitlements use.
	RunSeconds float64
	// Sandboxes is the number of distinct sandboxes that ever metered usage.
	Sandboxes int
}

// UsageDelta is one metering flush. Mirrors daemon.Usage, which cannot import the store.
type UsageDelta struct {
	VCPUSeconds      float64
	RAMGBSeconds     float64
	StorageGBSeconds float64
	RunSeconds       float64
	// NodeClass is the operator's hardware label (NODE_LABELS=class=...); empty
	// means unclassified. Pricing per class lives in httpapi, not here.
	NodeClass string
}

// RecordUsage appends a metering delta for a sandbox.
func (s *Store) RecordUsage(ctx context.Context, sandboxID, orgID string, u UsageDelta) error {
	return s.RecordUsageBatch(ctx, []UsageEntry{{SandboxID: sandboxID, OrgID: orgID, Delta: u}})
}

// UsageEntry is one sandbox's metered delta, for a batched insert.
type UsageEntry struct {
	SandboxID string
	OrgID     string
	Delta     UsageDelta
}

// clamped floors negative readings at zero so a caller cannot subtract from an
// org's recorded consumption.
func (u UsageDelta) clamped() UsageDelta {
	for _, f := range []*float64{&u.VCPUSeconds, &u.RAMGBSeconds, &u.StorageGBSeconds, &u.RunSeconds} {
		if *f < 0 {
			*f = 0
		}
	}
	return u
}

// RecordUsageBatch inserts metering deltas and the usage_totals rollup in one
// transaction so the two cannot drift. Entries are merged by (org, sandbox)
// first because Postgres rejects an upsert that hits the same row twice.
func (s *Store) RecordUsageBatch(ctx context.Context, entries []UsageEntry) error {
	if len(entries) == 0 {
		return nil
	}
	now := time.Now().Unix()
	type key struct{ org, sandbox string }
	var (
		detail strings.Builder
		args   = make([]any, 0, len(entries)*8)
		order  []key
		merged = map[key]UsageDelta{}
	)
	detail.WriteString(`INSERT INTO usage_records (sandbox_id, org_id, vcpu_seconds, ram_gb_seconds, storage_gb_seconds, run_seconds, node_class, recorded_at) VALUES `)
	for i, e := range entries {
		if i > 0 {
			detail.WriteString(", ")
		}
		detail.WriteString("(?, ?, ?, ?, ?, ?, ?, ?)")
		org := e.OrgID
		if org == "" {
			org = "default"
		}
		d := e.Delta.clamped()
		args = append(args, e.SandboxID, org,
			d.VCPUSeconds, d.RAMGBSeconds, d.StorageGBSeconds, d.RunSeconds, d.NodeClass, now)

		k := key{org, e.SandboxID}
		t, seen := merged[k]
		if !seen {
			order = append(order, k)
		}
		t.VCPUSeconds += d.VCPUSeconds
		t.RAMGBSeconds += d.RAMGBSeconds
		t.StorageGBSeconds += d.StorageGBSeconds
		t.RunSeconds += d.RunSeconds
		merged[k] = t
	}

	var rollup strings.Builder
	rollArgs := make([]any, 0, len(order)*7)
	rollup.WriteString(`INSERT INTO usage_totals (org_id, sandbox_id, vcpu_seconds, ram_gb_seconds, storage_gb_seconds, run_seconds, updated_at) VALUES `)
	for i, k := range order {
		if i > 0 {
			rollup.WriteString(", ")
		}
		rollup.WriteString("(?, ?, ?, ?, ?, ?, ?)")
		t := merged[k]
		rollArgs = append(rollArgs, k.org, k.sandbox,
			t.VCPUSeconds, t.RAMGBSeconds, t.StorageGBSeconds, t.RunSeconds, now)
	}
	rollup.WriteString(` ON CONFLICT (org_id, sandbox_id) DO UPDATE SET
		vcpu_seconds       = usage_totals.vcpu_seconds + excluded.vcpu_seconds,
		ram_gb_seconds     = usage_totals.ram_gb_seconds + excluded.ram_gb_seconds,
		storage_gb_seconds = usage_totals.storage_gb_seconds + excluded.storage_gb_seconds,
		run_seconds        = usage_totals.run_seconds + excluded.run_seconds,
		updated_at         = excluded.updated_at`)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // no-op after a successful Commit
	if _, err := tx.ExecContext(ctx, s.rebind(detail.String()), args...); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, s.rebind(rollup.String()), rollArgs...); err != nil {
		return err
	}
	return tx.Commit()
}

// UsageSummary is an org's lifetime metering, read from the usage_totals rollup
// so pruning usage_records does not change the answer.
func (s *Store) UsageSummary(ctx context.Context, orgID string) (UsageSummary, error) {
	if orgID == "" {
		orgID = "default"
	}
	var u UsageSummary
	err := s.queryRow(ctx,
		`SELECT COALESCE(SUM(vcpu_seconds),0), COALESCE(SUM(ram_gb_seconds),0),
		        COALESCE(SUM(storage_gb_seconds),0), COALESCE(SUM(run_seconds),0),
		        COUNT(*)
		 FROM usage_totals WHERE org_id = ?`, orgID,
	).Scan(&u.VCPUSeconds, &u.RAMGBSeconds, &u.StorageGBSeconds, &u.RunSeconds, &u.Sandboxes)
	return u, err
}

// ClassUsage is one hardware class's share of an org's consumption.
type ClassUsage struct {
	NodeClass string `json:"node_class"`
	UsageDelta
}

// UsageByClass splits an org's consumption by node class since a unix
// timestamp (0 = all time). Ordered by class for stable rendering.
func (s *Store) UsageByClass(ctx context.Context, orgID string, since int64) ([]ClassUsage, error) {
	if orgID == "" {
		orgID = "default"
	}
	return collectRows(ctx, s,
		`SELECT node_class,
		        COALESCE(SUM(vcpu_seconds),0), COALESCE(SUM(ram_gb_seconds),0),
		        COALESCE(SUM(storage_gb_seconds),0), COALESCE(SUM(run_seconds),0)
		 FROM usage_records WHERE org_id = ? AND recorded_at >= ?
		 GROUP BY node_class ORDER BY node_class`, []any{orgID, since},
		func(rows *sql.Rows, c *ClassUsage) error {
			return rows.Scan(&c.NodeClass, &c.VCPUSeconds, &c.RAMGBSeconds,
				&c.StorageGBSeconds, &c.RunSeconds)
		})
}

// ActiveHoursSince returns wall-clock running hours for an org since a unix
// timestamp. The caller picks the billing period start; the store has no opinion.
func (s *Store) ActiveHoursSince(ctx context.Context, orgID string, since int64) (float64, error) {
	if orgID == "" {
		orgID = "default"
	}
	var secs float64
	err := s.queryRow(ctx,
		`SELECT COALESCE(SUM(run_seconds),0) FROM usage_records
		 WHERE org_id = ? AND recorded_at >= ?`, orgID, since,
	).Scan(&secs)
	return secs / 3600, err
}
