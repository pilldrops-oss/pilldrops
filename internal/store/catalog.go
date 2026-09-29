package store

import (
	"context"
	"time"
)

// Fleet-wide admission counts and the atomic concurrency-slot claim.

// CountPersistentForOrg counts an org's persistent sandboxes fleet-wide. A
// persistent sandbox is marked by a zero expires_at; there is no "persistent"
// column. Paused rows still count because they hold their disk.
func (s *Store) CountPersistentForOrg(ctx context.Context, orgID string) (int, error) {
	var n int
	err := s.queryRow(ctx,
		`SELECT COUNT(*) FROM sandboxes WHERE org_id = ? AND expires_at <= 0 AND state NOT IN ('destroyed','error','lost')`,
		orgID,
	).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// UsageRow is one raw meter reading awaiting pricing.
type UsageRow struct {
	ID               int64
	OrgID            string
	VCPUSeconds      float64
	RAMGBSeconds     float64
	StorageGBSeconds float64
}

// slotWindowSecs is how long an unreleased slot keeps counting, so a create
// that died mid-flight frees its slot on its own.
const slotWindowSecs = 180

// ReserveSlot atomically claims one concurrency slot for an org, or reports
// that the cap is reached. Count and claim share one transaction serialized per
// org, and the count includes outstanding slots for creates still in flight.
func (s *Store) ReserveSlot(ctx context.Context, orgID, slotID string, cap int) (bool, error) {
	if cap <= 0 {
		return true, nil // unlimited
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	// Serialize admission per org. Postgres needs an advisory lock on the org
	// name (a row lock fails for a brand-new org); SQLite serializes writers already.
	if s.dialect == dialectPostgres {
		if _, err := tx.ExecContext(ctx, s.rebind(`SELECT pg_advisory_xact_lock(hashtext(?))`), orgID); err != nil {
			return false, err
		}
	}

	cutoff := nowUnix() - slotWindowSecs
	// Reap this org's expired claims while holding its lock; PurgeExpiredSlots
	// covers orgs that have gone quiet.
	if _, err := tx.ExecContext(ctx, s.rebind(
		`DELETE FROM admission_slots WHERE org_id = ? AND created_at <= ?`),
		orgID, cutoff); err != nil {
		return false, err
	}

	var live, held int
	if err := tx.QueryRowContext(ctx, s.rebind(
		`SELECT COUNT(*) FROM sandboxes WHERE org_id = ? AND state NOT IN ('destroyed','stopped','error','lost')`),
		orgID).Scan(&live); err != nil {
		return false, err
	}
	if err := tx.QueryRowContext(ctx, s.rebind(
		`SELECT COUNT(*) FROM admission_slots WHERE org_id = ? AND created_at > ?`),
		orgID, cutoff).Scan(&held); err != nil {
		return false, err
	}
	if live+held >= cap {
		return false, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, s.rebind(
		`INSERT INTO admission_slots (id, org_id, created_at) VALUES (?, ?, ?)`),
		slotID, orgID, nowUnix()); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// CountLiveForOrg counts an org's sandboxes that hold a concurrency place, the
// same rule ReserveSlot applies, without taking its lock.
func (s *Store) CountLiveForOrg(ctx context.Context, orgID string) (int, error) {
	var n int
	err := s.queryRow(ctx,
		`SELECT COUNT(*) FROM sandboxes WHERE org_id = ? AND state NOT IN ('destroyed','stopped','error','lost')`,
		orgID).Scan(&n)
	return n, err
}

// ReleaseSlot drops a claim once the create has finished. Best-effort: an
// unreleased slot ages out on its own.
func (s *Store) ReleaseSlot(ctx context.Context, slotID string) error {
	_, err := s.exec(ctx, `DELETE FROM admission_slots WHERE id = ?`, slotID)
	return err
}

// PurgeExpiredSlots deletes admission claims past the window, fleet-wide, and
// returns how many it removed.
func (s *Store) PurgeExpiredSlots(ctx context.Context) (int64, error) {
	res, err := s.exec(ctx, `DELETE FROM admission_slots WHERE created_at <= ?`, nowUnix()-slotWindowSecs)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil // driver does not report it; the delete still happened
	}
	return n, nil
}

// nowUnix is the store's single clock read.
func nowUnix() int64 { return time.Now().Unix() }
