package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"time"
)

// SetOrgPreviewSources replaces an org's preview source allow-list. An empty
// list clears the restriction: "no row" and "no CIDRs" both mean any peer may
// reach the org's previews, so a half-written policy never locks a customer out.
func (s *Store) SetOrgPreviewSources(ctx context.Context, orgID string, cidrs []string) error {
	if orgID == "" {
		orgID = "default"
	}
	if len(cidrs) == 0 {
		_, err := s.exec(ctx, `DELETE FROM org_preview_sources WHERE org_id = ?`, orgID)
		return err
	}
	for _, c := range cidrs {
		if _, err := netip.ParsePrefix(c); err != nil {
			// Validated here too, not only at the handler: an unparseable stored
			// CIDR would refuse every preview for the org.
			return fmt.Errorf("preview source %q is not a CIDR: %w", c, err)
		}
	}
	blob, err := json.Marshal(cidrs)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	_, err = s.exec(ctx,
		`INSERT INTO org_preview_sources (org_id, cidrs, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(org_id) DO UPDATE SET cidrs = ?, updated_at = ?`,
		orgID, string(blob), now, string(blob), now,
	)
	return err
}

// OrgPreviewSources returns an org's allow-list. An empty slice means no
// restriction; an error must never be read as one.
func (s *Store) OrgPreviewSources(ctx context.Context, orgID string) ([]string, error) {
	if orgID == "" {
		orgID = "default"
	}
	var blob string
	err := s.queryRow(ctx, `SELECT cidrs FROM org_preview_sources WHERE org_id = ?`, orgID).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	if err := json.Unmarshal([]byte(blob), &out); err != nil {
		return nil, fmt.Errorf("preview sources for %q are not readable (stored value %q): %w", orgID, blob, err)
	}
	return out, nil
}

// OrgsWithPreviewSources lists the orgs that have a restriction configured, so
// a deployment can be told at boot when the gate cannot discriminate. One row
// per restricted org, which is a handful at most.
func (s *Store) OrgsWithPreviewSources(ctx context.Context) ([]string, error) {
	return collectRows(ctx, s, `SELECT org_id FROM org_preview_sources ORDER BY org_id`, nil,
		func(rows *sql.Rows, org *string) error { return rows.Scan(org) })
}
