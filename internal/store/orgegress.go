package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/egress"
)

// SetOrgEgress replaces an org's egress override. nil clears the override so
// templates' own defaults apply; an empty &egress.Rules{} stores a policy that
// permits nothing.
func (s *Store) SetOrgEgress(ctx context.Context, orgID string, rules *egress.Rules) error {
	if orgID == "" {
		orgID = "default"
	}
	if rules == nil {
		_, err := s.exec(ctx, `DELETE FROM org_egress WHERE org_id = ?`, orgID)
		return err
	}
	if err := rules.Validate(); err != nil {
		// A stored but unparseable policy would fail every later create for the org.
		return fmt.Errorf("org egress rules for %q: %w", orgID, err)
	}
	blob, err := json.Marshal(rules)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	_, err = s.exec(ctx,
		`INSERT INTO org_egress (org_id, rules, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(org_id) DO UPDATE SET rules = ?, updated_at = ?`,
		orgID, string(blob), now, string(blob), now,
	)
	return err
}

// OrgEgress returns an org's override. ok=false means none is set, which the
// caller must read as "template default", never as "allow everything".
func (s *Store) OrgEgress(ctx context.Context, orgID string) (rules *egress.Rules, ok bool, err error) {
	if orgID == "" {
		orgID = "default"
	}
	var blob string
	row := s.queryRow(ctx, `SELECT rules FROM org_egress WHERE org_id = ?`, orgID)
	if err := row.Scan(&blob); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, err
	}
	var r egress.Rules
	if err := json.Unmarshal([]byte(blob), &r); err != nil {
		return nil, false, fmt.Errorf("org egress rules for %q are not readable (stored value %q): %w", orgID, blob, err)
	}
	return &r, true, nil
}
