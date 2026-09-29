package store

// The node's half of the artifact index: one read, on the create path. Kept
// apart from the writers because the node role is SELECT-only on template_builds.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrNoReadyBuild means the index has no published build for this template.
// Not an outage: the fetch path falls through to the compiled-in catalog.
var ErrNoReadyBuild = errors.New("store: no ready build for this template")

// ReadyBuildForTemplate returns the newest ready build id for a template.
// templateID must be canonical; the index knows nothing about aliases. The
// build_id tie-break matters: ready_at has second granularity, and two nodes
// must not boot different bytes for one template.
func (s *Store) ReadyBuildForTemplate(ctx context.Context, templateID string) (string, error) {
	if templateID == "" {
		return "", fmt.Errorf("%w: (empty template id)", ErrNoReadyBuild)
	}
	var buildID string
	err := s.queryRow(ctx, `
		SELECT build_id FROM template_builds
		WHERE template_id = ? AND status = 'ready' AND ready_at > 0
		ORDER BY ready_at DESC, build_id DESC
		LIMIT 1`, templateID).Scan(&buildID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("%w: %s", ErrNoReadyBuild, templateID)
	case err != nil:
		// A database error is an outage, not an absent template; keep it distinct.
		return "", fmt.Errorf("store: resolve ready build for %q: %w", templateID, err)
	}
	return buildID, nil
}
