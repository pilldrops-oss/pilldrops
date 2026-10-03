package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/store"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// A container node has no image directory: templates are images, resolved by
// the runtime's own image tier, and templates.Dir() is empty. The create path
// resolved a snapshot's template id through the squashfs tier only, so a
// snapshot captured on such a node — advertised as named_snapshot=true,
// reported ready — could never be booted from: every create of it came back
// "unknown template" before the runtime was ever asked.
//
// This asks the HTTP layer only, which is where the refusal was: a
// snapshot-backed create must reach the engine. What the engine then does with
// it is the daemon's test (snapshotcreate_test.go in internal/sandbox/daemon).
func TestASnapshotCreateIsNotRefusedByTheSquashfsTierAlone(t *testing.T) {
	srv, key := newTestServer(t)
	// The container-node shape, explicitly: no image directory at all.
	prev := templates.Dir()
	templates.SetDir("")
	t.Cleanup(func() { templates.SetDir(prev) })

	// The default org's own snapshot: another org's is refused before the tiers.
	id := store.SnapshotTemplateID("default", "golden")

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"`+id+`","snapshot":"golden"}`)
	if code == http.StatusBadRequest && strings.Contains(str(out, "error"), "unknown template") {
		t.Errorf("create from a snapshot = 400 %q: refused by the template tier before the "+
			"runtime's image tier was consulted, which is what makes a captured snapshot unbootable "+
			"on a container node", str(out, "error"))
	}

	// Positive control: the same id with no snapshot on the body is still an
	// unknown template, so the arm above is the snapshot's and not a hole.
	code, out = do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"`+id+`"}`)
	if code != http.StatusBadRequest || !strings.Contains(str(out, "error"), "unknown template") {
		t.Errorf("an ordinary create of an unknown template = %d %q, want 400 unknown template",
			code, str(out, "error"))
	}
}
