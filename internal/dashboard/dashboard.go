// Package dashboard serves the dashboard data plane (/dashboard/api/*): live
// stats, the sandbox list, config, and the SSE event/log streams the web UI
// consumes. Every route requires auth.
package dashboard

import (
	"net/http"
	"sync"

	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// Limits mirrors the daemon's enforced limits so the config panel reports real numbers.
type Limits struct {
	MaxActive int
	RateRPS   int
	RateBurst int
}

// Server serves the /dashboard/api data plane, backed by the daemon and store.
type Server struct {
	d         *daemon.Daemon
	st        *store.Store
	logs      *LogBroadcaster
	limits    Limits
	templates []string
	// consoleKey: see SetConsoleKey.
	consoleKey string

	done      chan struct{} // closed by Close; ends the SSE streams
	closeOnce sync.Once
}

// New builds a dashboard data-plane Server.
func New(d *daemon.Daemon, st *store.Store, logs *LogBroadcaster, limits Limits, templates ...string) *Server {
	if len(templates) == 0 {
		templates = []string{"python-3.11", "node-20"}
	}
	return &Server{d: d, st: st, logs: logs, limits: limits, templates: templates, done: make(chan struct{})}
}

// SetConsoleKey accepts the dashboard's signed account session in place of the
// key it was signed with, as httpapi.Server.SetConsoleKey does.
func (s *Server) SetConsoleKey(key string) { s.consoleKey = key }

// Close ends all open SSE streams. Wire it to http.Server.RegisterOnShutdown:
// an SSE stream never returns on its own, so it would hold Shutdown until the
// drain timeout. Idempotent.
func (s *Server) Close() { s.closeOnce.Do(func() { close(s.done) }) }

// Register mounts the data-plane routes onto mux. Every route is behind
// requireAuth because the data plane can destroy sandboxes.
func (s *Server) Register(mux *http.ServeMux) {
	mux.Handle("GET /dashboard/api/stats", s.requireAuth(s.handleStats))
	mux.Handle("GET /dashboard/api/sandboxes", s.requireAuth(s.handleSandboxes))
	mux.Handle("DELETE /dashboard/api/sandboxes/{id}", s.requireAuth(s.handleDestroy))
	mux.Handle("GET /dashboard/api/config", s.requireAuth(s.handleGetConfig))
	mux.Handle("PUT /dashboard/api/config", s.requireAuth(s.handlePutConfig))
	mux.Handle("GET /dashboard/api/events", s.requireAuth(s.handleEvents))
	mux.Handle("GET /dashboard/api/logs", s.requireAuth(s.handleLogs))
}
