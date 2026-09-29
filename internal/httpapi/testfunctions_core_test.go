//go:build core

package httpapi

import (
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// attachTestFunctions is a no-op in the core build, which has no function registry.
func attachTestFunctions(*Server, *daemon.Daemon, *store.Store, string) {}
