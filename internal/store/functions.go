package store

import (
	"context"
	"database/sql"
	"errors"
)

// FunctionRecord is a durable serverless-function definition keyed by (OrgID, Name).
type FunctionRecord struct {
	Name          string
	OrgID         string
	Template      string
	StartCmd      string
	Port          int
	VCPU          int // 0 = template default
	MemMiB        int
	MaxReplicas   int // 0 = default
	SandboxID     string
	Invokes       int64
	LastInvokedAt int64
	Env           string // JSON object of environment variables
	ColdStarts    int64
	CreatedAt     int64
	UpdatedAt     int64
}

// SaveFunction upserts a function row by (org_id, name). Env is encrypted at rest.
func (s *Store) SaveFunction(ctx context.Context, r FunctionRecord) error {
	env, err := s.crypt.seal(r.Env)
	if err != nil {
		return err
	}
	r.Env = env
	_, err = s.exec(ctx,
		`INSERT INTO functions
		   (name, org_id, template, start_cmd, port, vcpu, mem_mib, max_replicas, sandbox_id, invokes, last_invoked_at, env, cold_starts, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(org_id, name) DO UPDATE SET
		   template=excluded.template, start_cmd=excluded.start_cmd, port=excluded.port,
		   vcpu=excluded.vcpu, mem_mib=excluded.mem_mib, max_replicas=excluded.max_replicas,
		   sandbox_id=excluded.sandbox_id, invokes=excluded.invokes,
		   last_invoked_at=excluded.last_invoked_at, env=excluded.env,
		   cold_starts=excluded.cold_starts, updated_at=excluded.updated_at`,
		r.Name, r.OrgID, r.Template, r.StartCmd, r.Port, r.VCPU, r.MemMiB, r.MaxReplicas, r.SandboxID,
		r.Invokes, r.LastInvokedAt, r.Env, r.ColdStarts, r.CreatedAt, r.UpdatedAt,
	)
	return err
}

// GetFunction returns one function by (org, name).
func (s *Store) GetFunction(ctx context.Context, orgID, name string) (FunctionRecord, bool, error) {
	row := s.queryRow(ctx,
		`SELECT name, org_id, template, start_cmd, port, vcpu, mem_mib, max_replicas, sandbox_id, invokes, last_invoked_at, env, cold_starts, created_at, updated_at
		   FROM functions WHERE org_id = ? AND name = ?`, orgID, name)
	var r FunctionRecord
	err := row.Scan(&r.Name, &r.OrgID, &r.Template, &r.StartCmd, &r.Port, &r.VCPU, &r.MemMiB, &r.MaxReplicas, &r.SandboxID,
		&r.Invokes, &r.LastInvokedAt, &r.Env, &r.ColdStarts, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return FunctionRecord{}, false, nil
		}
		return FunctionRecord{}, false, err
	}
	if r.Env, err = s.crypt.open(r.Env); err != nil {
		return FunctionRecord{}, false, err
	}
	return r, true, nil
}

// LoadFunctions returns all functions for an org.
func (s *Store) LoadFunctions(ctx context.Context, orgID string) ([]FunctionRecord, error) {
	rows, err := s.query(ctx,
		`SELECT name, org_id, template, start_cmd, port, vcpu, mem_mib, max_replicas, sandbox_id, invokes, last_invoked_at, env, cold_starts, created_at, updated_at
		   FROM functions WHERE org_id = ? ORDER BY name`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FunctionRecord
	for rows.Next() {
		var r FunctionRecord
		if err := rows.Scan(&r.Name, &r.OrgID, &r.Template, &r.StartCmd, &r.Port, &r.VCPU, &r.MemMiB, &r.MaxReplicas, &r.SandboxID,
			&r.Invokes, &r.LastInvokedAt, &r.Env, &r.ColdStarts, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		if r.Env, err = s.crypt.open(r.Env); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteFunction removes a function row.
func (s *Store) DeleteFunction(ctx context.Context, orgID, name string) error {
	_, err := s.exec(ctx, `DELETE FROM functions WHERE org_id = ? AND name = ?`, orgID, name)
	return err
}
