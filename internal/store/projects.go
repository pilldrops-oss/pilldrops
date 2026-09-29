package store

import (
	"context"
	"database/sql"
	"encoding/json"
)

// Project is a durable unit of work that outlives the sandboxes it ran in.
// Projects are optional; a sandbox never needs one.
type Project struct {
	ID    string `json:"id"`
	OrgID string `json:"org_id"`
	Owner string `json:"owner,omitempty"`
	Name  string `json:"name"`
	// Plan is the serialised current intent, opaque to the store.
	Plan      string `json:"plan,omitempty"`
	CreatedAt int64  `json:"created_at"`
	LastRunAt int64  `json:"last_run_at,omitempty"`
	Archived  bool   `json:"archived,omitempty"`
}

// RunService ties one planned service to the sandbox that carried it.
type RunService struct {
	ServiceID string `json:"service_id"`
	SandboxID string `json:"sandbox_id"`
	Verdict   string `json:"verdict,omitempty"`
}

// Run is one execution of a project.
type Run struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	OrgID     string `json:"org_id"`
	// PlanSnapshot is frozen at execution; editing the project's plan never rewrites it.
	PlanSnapshot string       `json:"plan_snapshot,omitempty"`
	Services     []RunService `json:"services,omitempty"`
	StartedAt    int64        `json:"started_at"`
	EndedAt      int64        `json:"ended_at,omitempty"`
}

// SaveProject upserts a project by id.
func (s *Store) SaveProject(ctx context.Context, p Project) error {
	if p.OrgID == "" {
		p.OrgID = "default"
	}
	_, err := s.exec(ctx, `
		INSERT INTO projects (id, org_id, owner, name, plan, created_at, last_run_at, archived)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			name = EXCLUDED.name,
			plan = EXCLUDED.plan,
			last_run_at = EXCLUDED.last_run_at,
			archived = EXCLUDED.archived`,
		p.ID, p.OrgID, p.Owner, p.Name, p.Plan, p.CreatedAt, p.LastRunAt, p.Archived)
	return err
}

// GetProject returns one project, or nil when it does not exist in this org.
// Archived projects are returned.
func (s *Store) GetProject(ctx context.Context, orgID, id string) (*Project, error) {
	if orgID == "" {
		orgID = "default"
	}
	row := s.queryRow(ctx, `
		SELECT id, org_id, owner, name, plan, created_at, last_run_at, archived
		FROM projects WHERE org_id = ? AND id = ?`, orgID, id)
	var p Project
	if err := row.Scan(&p.ID, &p.OrgID, &p.Owner, &p.Name, &p.Plan, &p.CreatedAt, &p.LastRunAt, &p.Archived); err != nil {
		return nil, nil
	}
	return &p, nil
}

// ListProjects returns unarchived projects for an org, most recent activity first.
func (s *Store) ListProjects(ctx context.Context, orgID string, limit int) ([]Project, error) {
	if orgID == "" {
		orgID = "default"
	}
	if limit <= 0 {
		limit = 50
	}
	return collectRows(ctx, s, `
		SELECT id, org_id, owner, name, plan, created_at, last_run_at, archived
		FROM projects
		WHERE org_id = ? AND archived = ?
		ORDER BY last_run_at DESC, created_at DESC
		LIMIT ?`, []any{orgID, false, limit},
		func(rows *sql.Rows, p *Project) error {
			return rows.Scan(&p.ID, &p.OrgID, &p.Owner, &p.Name, &p.Plan, &p.CreatedAt, &p.LastRunAt, &p.Archived)
		})
}

// ArchiveProject hides a project from listings without deleting its run history.
func (s *Store) ArchiveProject(ctx context.Context, orgID, id string) error {
	if orgID == "" {
		orgID = "default"
	}
	_, err := s.exec(ctx, `UPDATE projects SET archived = ? WHERE org_id = ? AND id = ?`, true, orgID, id)
	return err
}

// RecordRun upserts a run by id and bumps the project's last_run_at.
func (s *Store) RecordRun(ctx context.Context, r Run) error {
	if r.OrgID == "" {
		r.OrgID = "default"
	}
	svc, err := json.Marshal(r.Services)
	if err != nil {
		return err
	}
	if _, err := s.exec(ctx, `
		INSERT INTO project_runs (id, project_id, org_id, plan_snapshot, services, started_at, ended_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			services = EXCLUDED.services,
			ended_at = EXCLUDED.ended_at`,
		r.ID, r.ProjectID, r.OrgID, r.PlanSnapshot, string(svc), r.StartedAt, r.EndedAt); err != nil {
		return err
	}
	// Only the activity marker moves; the project's plan is never touched here.
	_, err = s.exec(ctx, `UPDATE projects SET last_run_at = ? WHERE org_id = ? AND id = ?`, r.StartedAt, r.OrgID, r.ProjectID)
	return err
}

// EndRun closes a run with its finish time.
func (s *Store) EndRun(ctx context.Context, id string, endedAt int64) error {
	_, err := s.exec(ctx, `UPDATE project_runs SET ended_at = ? WHERE id = ?`, endedAt, id)
	return err
}

// ListRuns returns a project's runs, newest first.
func (s *Store) ListRuns(ctx context.Context, orgID, projectID string, limit int) ([]Run, error) {
	if orgID == "" {
		orgID = "default"
	}
	if limit <= 0 {
		limit = 50
	}
	return collectRows(ctx, s, `
		SELECT id, project_id, org_id, plan_snapshot, services, started_at, ended_at
		FROM project_runs
		WHERE org_id = ? AND project_id = ?
		ORDER BY started_at DESC
		LIMIT ?`, []any{orgID, projectID, limit},
		func(rows *sql.Rows, r *Run) error {
			var svc string
			if err := rows.Scan(&r.ID, &r.ProjectID, &r.OrgID, &r.PlanSnapshot, &svc, &r.StartedAt, &r.EndedAt); err != nil {
				return err
			}
			// A malformed services blob must not sink the whole list.
			_ = json.Unmarshal([]byte(svc), &r.Services)
			return nil
		})
}
