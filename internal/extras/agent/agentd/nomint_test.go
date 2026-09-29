package agentd

import (
	"go/build"
	"strings"
	"testing"
)

// The token minter must not be linked into the binary that runs inside the
// sandbox, so a prompt injection cannot mint a durable credential.
func TestAgentdCannotReachTheTokenMinter(t *testing.T) {
	forbidden := map[string]string{
		"github.com/pilldrops-oss/pilldrops/internal/auth/aat": "the token format and minter",
		// agentd has no database; its state is the journal, queue and budget file.
		"github.com/pilldrops-oss/pilldrops/internal/store": "the token database",
	}

	seen := map[string]bool{}
	var walk func(path string)
	walk = func(path string) {
		if seen[path] {
			return
		}
		seen[path] = true
		pkg, err := build.Import(path, "", 0)
		if err != nil {
			return
		}
		for _, imp := range pkg.Imports {
			if why, bad := forbidden[imp]; bad {
				t.Fatalf("agentd reaches %s (%s) via %s — a process inside the sandbox "+
					"must not be able to mint or read an agent access token", imp, why, path)
			}
			if strings.HasPrefix(imp, "github.com/pilldrops-oss/pilldrops/") {
				walk(imp)
			}
		}
	}
	walk("github.com/pilldrops-oss/pilldrops/internal/extras/agent/agentd")
	walk("github.com/pilldrops-oss/pilldrops/cmd/agentd")
}
