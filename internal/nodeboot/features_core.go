//go:build core

package nodeboot

import (
	"github.com/pilldrops-oss/pilldrops/internal/httpapi"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// startFeatures is a no-op in the core build. A build tag rather than a flag,
// so internal/extras/functions stays out of the link closure entirely.
func startFeatures(*httpapi.Server, *daemon.Daemon, *store.Store, string) {}
