package store

import (
	"context"
	"strings"
)

// ClassifySource maps a client tag to "agent" (CLI/MCP/SDK), "human" (web UI),
// or "" (unknown).
func ClassifySource(client string) string {
	switch strings.ToLower(strings.TrimSpace(client)) {
	case "cli", "mcp", "sdk", "api", "agent":
		return "agent"
	case "web", "portal", "ui", "browser", "dashboard":
		return "human"
	default:
		return ""
	}
}

// CreationsByClient counts an org's sandbox creations (active plus history)
// by raw client tag. An empty tag is folded into "unknown".
func (s *Store) CreationsByClient(ctx context.Context, orgID string) (map[string]int, error) {
	if orgID == "" {
		orgID = "default"
	}
	rows, err := s.query(ctx,
		`SELECT client, COUNT(*) AS c FROM (
		   SELECT client FROM sandboxes WHERE org_id = ?
		   UNION ALL
		   SELECT client FROM sandbox_history WHERE org_id = ?
		 ) t GROUP BY client`, orgID, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var name string
		var c int
		if err := rows.Scan(&name, &c); err != nil {
			return nil, err
		}
		if name == "" {
			name = "unknown"
		}
		out[name] += c
	}
	return out, rows.Err()
}
