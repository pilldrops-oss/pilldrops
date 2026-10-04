package httpapi

import (
	"context"
	"net/http"

	"github.com/pilldrops-oss/pilldrops/internal/cron"
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
	go cron.NewSweeper(s.st, s.cronRunner(), nil).Run(ctx)
	return cancel
}
