package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Scheduled runs. next_run_at is denormalised so the sweeper selects due work
// with one indexed query; the caller owns the schedule arithmetic and this file
// knows nothing about cron syntax.

// maxCronOutput caps the stored output of a run.
const maxCronOutput = 4096

// CronJob is one scheduled command against one workstation.
type CronJob struct {
	ID        string `json:"id"`
	OrgID     string `json:"org_id"`
	SandboxID string `json:"sandbox_id"`
	Name      string `json:"name"`
	Schedule  string `json:"schedule"`
	// Timezone is the IANA zone the schedule is read in; empty means UTC.
	Timezone string `json:"timezone"`
	Command  string `json:"command"`
	// AgentMode decides what Command is: empty means a shell command, "guided"
	// or "autonomous" means a prompt submitted to the agent as a turn.
	AgentMode string `json:"agent_mode,omitempty"`
	// Kind decides who runs the job: "shell" (the default, and every row
	// written before this column existed) hands Command to the sandbox,
	// "backup" hands the sandbox to the daemon's capture and ignores Command.
	// Stored rather than inferred from the name, so the sweeper never guesses.
	Kind      string `json:"kind,omitempty"`
	Enabled   bool   `json:"enabled"`
	NextRunAt int64  `json:"next_run_at"`
	LastRunAt int64  `json:"last_run_at"`
	// LastStatus is "ok", "failed" (the command's exit) or "error" (it never ran).
	LastStatus string `json:"last_status"`
	LastOutput string `json:"last_output"`
	CreatedAt  int64  `json:"created_at"`
}

func newCronID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "cron_" + hex.EncodeToString(b), nil
}

// CreateCronJob stores a job. A zero nextRunAt means the job is never selected.
func (s *Store) CreateCronJob(ctx context.Context, j CronJob, nextRunAt int64) (CronJob, error) {
	if j.OrgID == "" || j.SandboxID == "" {
		return CronJob{}, fmt.Errorf("org and sandbox are required")
	}
	if j.Kind == "" {
		j.Kind = CronKindShell
	}
	if j.Kind != CronKindShell && j.Kind != CronKindBackup {
		return CronJob{}, fmt.Errorf("kind must be %q or %q", CronKindShell, CronKindBackup)
	}
	// A backup job has nothing to run inside the sandbox, and the column is NOT
	// NULL; it carries the kind rather than an empty string.
	if j.Kind == CronKindBackup && strings.TrimSpace(j.Command) == "" {
		j.Command = CronKindBackup
	}
	if strings.TrimSpace(j.Command) == "" {
		return CronJob{}, fmt.Errorf("command is required")
	}
	// This value decides whether Command goes to a shell or an agent; never let
	// the sweeper guess.
	switch j.AgentMode {
	case "", "guided", "autonomous":
	default:
		return CronJob{}, fmt.Errorf("agent_mode must be empty, \"guided\" or \"autonomous\"")
	}
	id, err := newCronID()
	if err != nil {
		return CronJob{}, err
	}
	j.ID, j.CreatedAt, j.NextRunAt, j.Enabled = id, time.Now().Unix(), nextRunAt, true
	_, err = s.exec(ctx,
		`INSERT INTO cron_jobs (id, org_id, sandbox_id, name, schedule, timezone, command, agent_mode, kind, enabled, next_run_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, TRUE, ?, ?)`,
		j.ID, j.OrgID, j.SandboxID, j.Name, j.Schedule, j.Timezone, j.Command, j.AgentMode, j.Kind,
		j.NextRunAt, j.CreatedAt,
	)
	if err != nil {
		return CronJob{}, err
	}
	return j, nil
}

// CronKindShell and CronKindBackup are the values of CronJob.Kind.
const (
	CronKindShell  = "shell"
	CronKindBackup = "backup"
)

const cronCols = `id, org_id, sandbox_id, name, schedule, timezone, command, agent_mode, kind, enabled, next_run_at, last_run_at, last_status, last_output, created_at`

func scanCron(sc rowScanner) (CronJob, error) {
	var j CronJob
	var enabled bool
	err := sc.Scan(&j.ID, &j.OrgID, &j.SandboxID, &j.Name, &j.Schedule, &j.Timezone, &j.Command,
		&j.AgentMode, &j.Kind, &enabled, &j.NextRunAt, &j.LastRunAt, &j.LastStatus, &j.LastOutput, &j.CreatedAt)
	j.Enabled = enabled
	return j, err
}

// ListCronJobs returns an org's jobs, newest first.
func (s *Store) ListCronJobs(ctx context.Context, orgID string) ([]CronJob, error) {
	return collectRows(ctx, s,
		`SELECT `+cronCols+` FROM cron_jobs WHERE org_id = ? ORDER BY created_at DESC`,
		[]any{orgID}, scanInto(scanCron))
}

// CronJobByID returns one job scoped to its org.
func (s *Store) CronJobByID(ctx context.Context, orgID, id string) (CronJob, bool) {
	j, err := scanCron(s.queryRow(ctx, `SELECT `+cronCols+` FROM cron_jobs WHERE id = ? AND org_id = ?`, id, orgID))
	if err != nil {
		return CronJob{}, false
	}
	return j, true
}

// DueCronJobs returns enabled jobs whose next run has arrived, on sandboxes
// nodeID owns or no node has stamped yet. Every node of a fleet sweeps this one
// table and only the owner can exec into a sandbox: a sibling that won the claim
// recorded "sandbox not found" instead. An empty nodeID (unclustered) sees every job.
func (s *Store) DueCronJobs(ctx context.Context, nodeID string, now int64, limit int) ([]CronJob, error) {
	if limit <= 0 {
		limit = 100
	}
	if nodeID == "" {
		return collectRows(ctx, s,
			`SELECT `+cronCols+` FROM cron_jobs
			 WHERE enabled AND next_run_at > 0 AND next_run_at <= ?
			 ORDER BY next_run_at LIMIT ?`,
			[]any{now, limit}, scanInto(scanCron))
	}
	return collectRows(ctx, s,
		`SELECT `+cronCols+` FROM cron_jobs
		 WHERE enabled AND next_run_at > 0 AND next_run_at <= ?
		   AND sandbox_id IN (SELECT id FROM sandboxes WHERE node_id = ? OR node_id = '')
		 ORDER BY next_run_at LIMIT ?`,
		[]any{now, nodeID, limit}, scanInto(scanCron))
}

// ClaimCronJob moves a job's next_run_at forward and reports whether this
// caller was the one that moved it. Matching on the value the sweeper read is
// what stops two replicas firing the same job.
func (s *Store) ClaimCronJob(ctx context.Context, id string, expectedNextRun, newNextRun int64) (bool, error) {
	res, err := s.exec(ctx,
		`UPDATE cron_jobs SET next_run_at = ? WHERE id = ? AND next_run_at = ?`,
		newNextRun, id, expectedNextRun)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// RecordCronRun stores the outcome of a run, keeping the tail of the output.
func (s *Store) RecordCronRun(ctx context.Context, id, status, output string, ranAt int64) error {
	if len(output) > maxCronOutput {
		output = output[len(output)-maxCronOutput:]
	}
	_, err := s.exec(ctx,
		`UPDATE cron_jobs SET last_run_at = ?, last_status = ?, last_output = ? WHERE id = ?`,
		ranAt, status, output, id)
	return err
}

// SetCronEnabled pauses or resumes a job; next_run_at is left intact.
func (s *Store) SetCronEnabled(ctx context.Context, orgID, id string, enabled bool) (bool, error) {
	res, err := s.exec(ctx, `UPDATE cron_jobs SET enabled = ? WHERE id = ? AND org_id = ?`, enabled, id, orgID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// DeleteCronJob removes a job, org-scoped.
func (s *Store) DeleteCronJob(ctx context.Context, orgID, id string) (bool, error) {
	res, err := s.exec(ctx, `DELETE FROM cron_jobs WHERE id = ? AND org_id = ?`, id, orgID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// DeleteCronJobsForSandbox removes every job pointing at a destroyed sandbox.
func (s *Store) DeleteCronJobsForSandbox(ctx context.Context, sandboxID string) error {
	_, err := s.exec(ctx, `DELETE FROM cron_jobs WHERE sandbox_id = ?`, sandboxID)
	return err
}
