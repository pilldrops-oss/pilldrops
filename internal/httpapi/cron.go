package httpapi

import (
	"context"
	"errors"

	"github.com/pilldrops-oss/pilldrops/internal/cron"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// The scheduler fires backup schedules only; internal/cron owns the schedule
// math and the sweeper, backupschedulecron.go writes the jobs.

// StartCronSweeper begins firing scheduled backups and returns a stop function.
// Not started from New, so short-lived processes never capture sandboxes unasked.
func (s *Server) StartCronSweeper() (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	go s.cronSweeper().Run(ctx)
	return cancel
}

func (s *Server) cronSweeper() *cron.Sweeper {
	return cron.NewSweeper(nodeCronJobs{s.st, s.nodeID}, daemonBackupRunner{s}, nil)
}

// nodeCronJobs scopes the sweep to the jobs this node can run.
type nodeCronJobs struct {
	*store.Store
	node string
}

func (n nodeCronJobs) DueCronJobs(ctx context.Context, now int64, limit int) ([]store.CronJob, error) {
	return n.Store.DueCronJobs(ctx, n.node, now, limit)
}

// daemonBackupRunner captures a scheduled backup through the daemon.
type daemonBackupRunner struct{ s *Server }

// RunBackup implements cron.BackupRunner. It returns as soon as the sandbox is
// running again, so a sweep slot is held for the pause and not for the upload.
func (d daemonBackupRunner) RunBackup(ctx context.Context, _, sandboxID string) (string, error) {
	started, err := d.s.d.Backup(ctx, sandboxID)
	if errors.Is(err, rt.ErrNotFound) && d.ownedElsewhere(ctx, sandboxID) {
		return "", errOwnerDown
	}
	if err != nil {
		return "", err
	}
	return started.ID, nil
}

var errOwnerDown = errors.New("unavailable: owning node is down")

// ownedElsewhere reports whether the sandbox's row names another node as its owner.
func (d daemonBackupRunner) ownedElsewhere(ctx context.Context, sandboxID string) bool {
	rec, ok, err := d.s.st.SandboxByID(ctx, sandboxID)
	return err == nil && ok && rec.NodeID != "" && rec.NodeID != d.s.nodeID
}
