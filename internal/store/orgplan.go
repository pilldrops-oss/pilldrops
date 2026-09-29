package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// OrgPlan is an org's assigned limit preset. An org with no row is unlimited.
type OrgPlan struct {
	OrgID     string
	PlanID    string
	UpdatedAt int64
}

// SetPlan assigns a preset to an org, creating the row if needed.
func (s *Store) SetPlan(ctx context.Context, orgID, planID string) error {
	if orgID == "" {
		orgID = "default"
	}
	now := time.Now().Unix()
	_, err := s.exec(ctx,
		`INSERT INTO org_plans (org_id, plan_id, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(org_id) DO UPDATE SET plan_id = ?, updated_at = ?`,
		orgID, planID, now, planID, now,
	)
	return err
}

// PlanFor returns the org's assigned preset. ok=false means no limits assigned.
func (s *Store) PlanFor(ctx context.Context, orgID string) (OrgPlan, bool, error) {
	if orgID == "" {
		orgID = "default"
	}
	var p OrgPlan
	err := s.queryRow(ctx,
		`SELECT org_id, plan_id, updated_at FROM org_plans WHERE org_id = ?`, orgID,
	).Scan(&p.OrgID, &p.PlanID, &p.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return OrgPlan{}, false, nil
		}
		return OrgPlan{}, false, err
	}
	return p, true, nil
}

// ClearPlan removes an org's assignment, returning it to unlimited.
func (s *Store) ClearPlan(ctx context.Context, orgID string) error {
	if orgID == "" {
		orgID = "default"
	}
	_, err := s.exec(ctx, `DELETE FROM org_plans WHERE org_id = ?`, orgID)
	return err
}

// firstNonEmpty returns a when set, else b.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
