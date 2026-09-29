package templates

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// The client keeps AGENT_CONFIG as a fallback and prefers the server copy, so
// drift is invisible in normal use. A missing client file is a failure, not a skip.
func TestClientAgentConfigAgreesWithTheManifest(t *testing.T) {
	// AGENT_CONFIG lives in lib/launchAgents.ts; components/launchConfig.tsx only
	// re-exports it. Reading the barrel matched zero entries, which the count
	// check below turned into a loud failure rather than a silent pass.
	raw, err := os.ReadFile(filepath.Join("..", "..", "web", "src", "lib", "launchAgents.ts"))
	if err != nil {
		t.Fatalf("client not present: %v — this is the only check guarding web/src/lib/launchAgents.ts's AGENT_CONFIG against drift from the manifest; it must not be silently skipped", err)
	}
	src := string(raw)

	// The key may be quoted: a hyphenated id is not a valid unquoted JS identifier.
	block := regexp.MustCompile(`(?s)\n  '?([a-z0-9-]+)'?: \{(.*?)\n  \},`)
	field := func(body, name string) string {
		m := regexp.MustCompile(name + `:\s*'([^']*)'`).FindStringSubmatch(body)
		if m == nil {
			return ""
		}
		return m[1]
	}

	seen := map[string]bool{}
	for _, m := range block.FindAllStringSubmatch(src, -1) {
		id, body := m[1], m[2]
		meta, ok := agentMeta[id]
		if !ok {
			continue
		}
		seen[id] = true
		for _, c := range []struct{ name, client, server string }{
			{"keyEnv", field(body, "keyEnv"), meta.KeyEnv},
			{"baseUrlEnv", field(body, "baseUrlEnv"), meta.BaseURLEnv},
			{"modelEnv", field(body, "modelEnv"), meta.ModelEnv},
			{"installCmd", field(body, "installCmd"), meta.InstallCmd},
		} {
			if c.client != c.server {
				t.Errorf("%s.%s: client %q, manifest %q", id, c.name, c.client, c.server)
			}
		}
	}
	for id := range agentMeta {
		if !seen[id] {
			t.Errorf("%s: in agentMeta but no matching AGENT_CONFIG entry found in web/src/lib/launchAgents.ts — the client fallback did not grow with the manifest", id)
		}
	}
	if len(seen) != len(agentMeta) {
		t.Fatalf("matched %d agent entries in launchAgents.ts, want %d (len(agentMeta)) — either the manifest and the client have drifted, or the parser regex broke and silently matched fewer entries than it should", len(seen), len(agentMeta))
	}
}
