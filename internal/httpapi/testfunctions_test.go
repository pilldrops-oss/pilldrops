//go:build !core

package httpapi

import (
	"path/filepath"

	"github.com/pilldrops-oss/pilldrops/internal/extras/functions"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// attachTestFunctions wires the function registry into a test server.
func attachTestFunctions(s *Server, d *daemon.Daemon, st *store.Store, dir string) {
	s.SetFunctions(functions.NewRegistry(d, st, filepath.Join(dir, "functions")))
}
