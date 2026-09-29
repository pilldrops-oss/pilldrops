package store

import (
	"context"
	"strings"
	"time"
)

// SetSandboxIntegrations replaces the enabled MCP server set for a sandbox.
// Replace rather than merge, so an unticked server is actually gone.
func (s *Store) SetSandboxIntegrations(ctx context.Context, sandboxID, orgID string, ids []string) error {
	joined := strings.Join(ids, ",")
	if _, err := s.exec(ctx, `DELETE FROM sandbox_integrations WHERE sandbox_id = ?`, sandboxID); err != nil {
		return err
	}
	_, err := s.exec(ctx,
		`INSERT INTO sandbox_integrations (sandbox_id, org_id, servers, updated_at) VALUES (?, ?, ?, ?)`,
		sandboxID, orgID, joined, time.Now().Unix())
	return err
}

// SandboxIntegrations returns the enabled ids, empty when none are set.
func (s *Store) SandboxIntegrations(ctx context.Context, sandboxID string) ([]string, error) {
	var joined string
	err := s.queryRow(ctx, `SELECT servers FROM sandbox_integrations WHERE sandbox_id = ?`, sandboxID).Scan(&joined)
	if err != nil || strings.TrimSpace(joined) == "" {
		// A missing row means none enabled, not an error.
		return []string{}, nil
	}
	return strings.Split(joined, ","), nil
}

// DeleteSandboxIntegrations drops a sandbox's selection.
func (s *Store) DeleteSandboxIntegrations(ctx context.Context, sandboxID string) error {
	_, err := s.exec(ctx, `DELETE FROM sandbox_integrations WHERE sandbox_id = ?`, sandboxID)
	return err
}
