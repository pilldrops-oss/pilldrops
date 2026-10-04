package httpapi

import (
	"context"
	"net/http"

	"github.com/pilldrops-oss/pilldrops/internal/cron"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// registerCronRoutes mounts the scheduled-run routes.
func (s *Server) registerCronRoutes(route func(string, http.Handler)) {
	route("POST /v1/cron", s.auth(s.handleCronCreate))
	route("GET /v1/cron", s.auth(s.handleCronList))
	route("DELETE /v1/cron/{id}", s.auth(s.handleCronDelete))
	route("POST /v1/cron/{id}/toggle", s.auth(s.handleCronToggle))
	route("POST /v1/cron/{id}/run", s.auth(s.handleCronRunNow))
}

// StartCronSweeper begins firing scheduled jobs and returns a stop function.
// Not started from New, so short-lived processes never run customer commands unasked.
func (s *Server) StartCronSweeper() (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	go s.cronSweeper().Run(ctx)
	return cancel
}

func (s *Server) cronSweeper() *cron.Sweeper {
	return cron.NewSweeper(nodeCronJobs{s.st, s.nodeID}, s.cronRunner(), nil)
}

// nodeCronJobs scopes the sweep to the jobs this node can run.
type nodeCronJobs struct {
	*store.Store
	node string
}

func (n nodeCronJobs) DueCronJobs(ctx context.Context, now int64, limit int) ([]store.CronJob, error) {
	return n.Store.DueCronJobs(ctx, n.node, now, limit)
}
