package store

import (
	"context"
	"database/sql"
	"log/slog"
)

// sessionGapSecs is the idle gap after which a re-auth counts as a new login.
const sessionGapSecs = 1800

// UserRow is one god-view list entry: login stats plus per-owner aggregates.
// owner_id doubles as the org_id in every other table.
type UserRow struct {
	OwnerID         string `json:"owner_id"`
	Email           string `json:"email"`
	FirstSeen       int64  `json:"first_seen"`
	LastSeen        int64  `json:"last_seen"`
	LoginCount      int64  `json:"login_count"`
	ActiveSandboxes int    `json:"active_sandboxes"`
	TotalCreated    int    `json:"total_created"`
	Functions       int    `json:"functions"`
	Plan            string `json:"plan"`
}

// UpsertLogin records or refreshes a login. login_count increments only when
// the idle gap since the previous last_seen exceeds sessionGapSecs.
func (s *Store) UpsertLogin(ctx context.Context, now int64, ownerID, userID, email string) error {
	if ownerID == "" {
		return nil
	}
	_, err := s.exec(ctx,
		`INSERT INTO users (owner_id, user_id, email, first_seen, last_seen, login_count)
		 VALUES (?, ?, ?, ?, ?, 1)
		 ON CONFLICT (owner_id) DO UPDATE SET
		   last_seen   = excluded.last_seen,
		   email       = excluded.email,
		   login_count = users.login_count + CASE
		     WHEN excluded.last_seen - users.last_seen > ? THEN 1 ELSE 0 END`,
		ownerID, userID, email, now, now, sessionGapSecs,
	)
	return err
}

// queryUsers returns one owner when ownerID is set, else all users newest-active first.
func (s *Store) queryUsers(ctx context.Context, ownerID string, limit int) ([]UserRow, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	where, args := "", []any{}
	if ownerID != "" {
		where = "WHERE u.owner_id = ?"
		args = append(args, ownerID)
	}
	args = append(args, limit)
	return collectRows(ctx, s,
		`SELECT u.owner_id, u.email, u.first_seen, u.last_seen, u.login_count,
		   (SELECT COUNT(*) FROM sandboxes sb WHERE sb.org_id = u.owner_id),
		   (SELECT COUNT(*) FROM sandbox_history h WHERE h.org_id = u.owner_id),
		   (SELECT COUNT(*) FROM functions f WHERE f.org_id = u.owner_id),
		   COALESCE((SELECT c.plan_id FROM org_plans c WHERE c.org_id = u.owner_id), '')
		 FROM users u `+where+`
		 ORDER BY u.last_seen DESC LIMIT ?`, args,
		func(rows *sql.Rows, u *UserRow) error {
			var destroyed int
			if err := rows.Scan(&u.OwnerID, &u.Email, &u.FirstSeen, &u.LastSeen,
				&u.LoginCount, &u.ActiveSandboxes, &destroyed, &u.Functions,
				&u.Plan); err != nil {
				return err
			}
			u.TotalCreated = u.ActiveSandboxes + destroyed
			return nil
		})
}

// ListUsers returns every user with aggregates, newest-active first. Cross-org; operator only.
func (s *Store) ListUsers(ctx context.Context, limit int) ([]UserRow, error) {
	return s.queryUsers(ctx, "", limit)
}

// UserDetail is the god-view drill-down for one owner.
type UserDetail struct {
	User      UserRow            `json:"user"`
	Active    []ActiveSandboxRow `json:"active"`
	History   []HistoryRecord    `json:"history"`
	Functions []FunctionRecord   `json:"functions"`
	Usage     UsageSummary       `json:"usage"`
	Events    []EventRecord      `json:"events"`
	Audit     []AuditEntry       `json:"audit"`
	Plan      *OrgPlan           `json:"plan"`
	// ByClient is the client tag to creation count breakdown.
	ByClient map[string]int `json:"by_client"`
}

// GetUserDetail returns the drill-down for ownerID, or (nil, nil) when there is no users row.
func (s *Store) GetUserDetail(ctx context.Context, ownerID string) (*UserDetail, error) {
	rows, err := s.queryUsers(ctx, ownerID, 1)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	d := &UserDetail{User: rows[0]}
	if d.Active, err = s.ActiveSandboxRowsByOrg(ctx, ownerID); err != nil {
		return nil, err
	}
	if d.History, err = s.ListHistory(ctx, ownerID, 200); err != nil {
		return nil, err
	}
	if d.Functions, err = s.LoadFunctions(ctx, ownerID); err != nil {
		return nil, err
	}
	if d.Usage, err = s.UsageSummary(ctx, ownerID); err != nil {
		return nil, err
	}
	if d.Events, err = s.ListEvents(ctx, ownerID, 100); err != nil {
		return nil, err
	}
	if d.Audit, err = s.ListAudit(ctx, ownerID, 100); err != nil {
		return nil, err
	}
	if p, ok, perr := s.PlanFor(ctx, ownerID); perr != nil {
		return nil, perr
	} else if ok {
		d.Plan = &p
	}
	// A failed breakdown must not fail the whole drill-down.
	if bc, cerr := s.CreationsByClient(ctx, ownerID); cerr != nil {
		slog.ErrorContext(ctx, "creations-by-client lookup failed, serving an empty breakdown",
			"component", "store", "owner", ownerID, "err", cerr)
		d.ByClient = map[string]int{}
	} else {
		d.ByClient = bc
	}
	return d, nil
}
