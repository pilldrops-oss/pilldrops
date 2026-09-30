//go:build !core

package nodeboot

import (
	"github.com/pilldrops-oss/pilldrops/internal/httpapi"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// startFeatures starts everything outside the runtime core: the agent mirror
// and spend guard. Started here rather than in httpapi.New so tests get the
// HTTP surface without background goroutines.
func startFeatures(api *httpapi.Server, d *daemon.Daemon, st *store.Store, dataDir string) {
	// Both are safe on every node without leader election: the mirror writes
	// idempotently and the spend guard has no shared state to race.
	api.StartAgentMirror()
	api.StartSpendGuard()
}
