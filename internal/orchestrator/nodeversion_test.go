package orchestrator

import (
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

// The heartbeat carries the version; a fleet view without it reads as unknown
// until an upgrade job has run.
func TestNodeViewReportsTheHeartbeatsVersion(t *testing.T) {
	if got := nodeView(&cluster.Node{ID: "n1", Version: "v1.2.3"}, time.Now())["version"]; got != "v1.2.3" {
		t.Errorf("version = %v", got)
	}
}
