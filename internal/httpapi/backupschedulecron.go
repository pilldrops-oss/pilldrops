//go:build !core

package httpapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cron"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// The half of the backup schedule that needs the scheduler. A core build has
// no sweeper to fire a job, so it links none of this and refuses a schedule
// outright rather than storing one nothing would ever run — see
// backupschedulenocron.go.

// scheduleGapError measures the actual gap between two consecutive fires and
// refuses one under the floor. Measured, not parsed: "*/5 * * * *" and a
// period of "5m" are the same schedule written two ways, and only the fires
// tell the truth about either.
func scheduleGapError(expr string, from time.Time) error {
	sched, err := cron.Parse(expr)
	if err != nil {
		return fmt.Errorf("schedule %q is not valid: %w", expr, err)
	}
	first := sched.Next(from)
	if first.IsZero() {
		return fmt.Errorf("schedule %q never fires", expr)
	}
	second := sched.Next(first)
	if second.IsZero() {
		return fmt.Errorf("schedule %q fires only once", expr)
	}
	if gap := second.Sub(first); gap < BackupFloor {
		return fmt.Errorf("%w (this one fires every %s)", ErrScheduleTooFrequent, gap)
	}
	return nil
}

// applyBackupSchedule creates, replaces or deletes a sandbox's backup job.
// "off" as the period removes it.
func (s *Server) applyBackupSchedule(ctx context.Context, orgID, sandboxID string, spec BackupSchedule) error {
	if spec.Period != "" && spec.Cron != "" {
		return errors.New("give a backup period or a cron expression, not both")
	}
	retention, err := parseRetention(spec.Retention)
	if err != nil {
		return err
	}
	off := strings.EqualFold(spec.Period, "off") || strings.EqualFold(spec.Cron, "off")
	if spec.Period == "" && spec.Cron == "" {
		// Retention without a schedule has nothing to apply to: the expiry is
		// stamped on a backup when it is created, and nothing creates one.
		if retention > 0 {
			return errors.New("a backup retention needs a backup period; set backups.period first")
		}
		return nil
	}

	// Replace rather than update: one name, one job, and a changed schedule
	// must not inherit the old one's next_run_at.
	if err := s.deleteBackupJob(ctx, orgID, sandboxID); err != nil {
		return err
	}
	if off {
		return nil
	}

	expr := spec.Cron
	if expr == "" {
		if expr, err = cronForPeriod(spec.Period); err != nil {
			return err
		}
	}
	now := time.Now()
	if err := scheduleGapError(expr, now); err != nil {
		return err
	}
	sched, err := cron.Parse(expr)
	if err != nil {
		return fmt.Errorf("schedule %q is not valid: %w", expr, err)
	}
	// Turning a schedule on is a promise to run backups. A node with no object
	// store answers 501 to every backup it is asked for, and its own startup log
	// says every schedule here fails every fire — so storing one and reporting
	// it enabled was the one answer that could not be acted on. After the
	// schedule itself is checked, so a caller who got both wrong is still told
	// about their own input first.
	if err := s.d.BackupsReady(); err != nil {
		return err
	}
	if _, err := s.st.CreateCronJob(ctx, store.CronJob{
		OrgID: orgID, SandboxID: sandboxID, Name: backupJobName,
		Schedule: expr, Kind: store.CronKindBackup,
	}, sched.Next(now).Unix()); err != nil {
		return err
	}
	// The daemon stamps expires_at on each new backup, so it needs the number
	// the moment a capture runs, not at the next config read. It persists it
	// too: the job row above carries no retention, so this is the only thing
	// that survives a node restart.
	return s.d.SetBackupRetention(ctx, sandboxID, retention)
}

// deleteBackupJob removes the sandbox's backup job if it has one.
func (s *Server) deleteBackupJob(ctx context.Context, orgID, sandboxID string) error {
	jobs, err := s.st.ListCronJobs(ctx, orgID)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if j.SandboxID == sandboxID && j.Kind == store.CronKindBackup {
			if _, err := s.st.DeleteCronJob(ctx, orgID, j.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// backupScheduleView reports a sandbox's schedule for the config GET.
func (s *Server) backupScheduleView(ctx context.Context, orgID, sandboxID string) map[string]any {
	jobs, err := s.st.ListCronJobs(ctx, orgID)
	if err != nil {
		return nil
	}
	for _, j := range jobs {
		if j.SandboxID == sandboxID && j.Kind == store.CronKindBackup {
			v := map[string]any{
				"cron": j.Schedule, "enabled": j.Enabled,
				"next_run_at": j.NextRunAt, "last_status": j.LastStatus,
			}
			// Omitted when unset: absent means BACKUPS_RETAIN_DAYS, and a
			// rendered "0s" would read as "kept forever", which is what 0
			// means to the deployment default but not to a sandbox.
			if retain := s.d.BackupRetention(sandboxID); retain > 0 {
				v["retention"] = retain.String()
			}
			return v
		}
	}
	return nil
}
