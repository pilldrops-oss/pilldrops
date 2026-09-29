//go:build !core

package nodeboot

import (
	"path/filepath"

	"github.com/pilldrops-oss/pilldrops/internal/extras/functions"
	"github.com/pilldrops-oss/pilldrops/internal/httpapi"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// startFeatures starts everything outside the runtime core: the function
// registry, cron sweeper, agent mirror and spend guard. Started here rather
// than in httpapi.New so tests get the HTTP surface without background goroutines.
func startFeatures(api *httpapi.Server, d *daemon.Daemon, st *store.Store, dataDir string) {
	api.SetFunctions(functions.NewRegistry(d, st, filepath.Join(dataDir, "functions")))
	// Both are safe on every node without leader election: the sweeper claims
	// jobs with a conditional update and the mirror writes idempotently.
	api.StartCronSweeper()
	api.StartAgentMirror()
	api.StartSpendGuard()
}
