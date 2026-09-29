package docker

import (
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// TestEveryAgentHasARenderedImage checks every manifest agent reaches
// enrichedDockerfiles. Contents are not compared: they are rendered from the
// same map, so that comparison would be tautological.
func TestEveryAgentHasARenderedImage(t *testing.T) {
	for id := range templates.AgentMetaByID() {
		if _, ok := enrichedDockerfiles[id]; !ok {
			t.Errorf("%s has a manifest but no image — the CLI would be missing from the sandbox", id)
		}
	}
}

// TestAgentDockerfileRendersFromMeta uses an inline AgentMeta so the renderer
// is checked independently of the manifest.
func TestAgentDockerfileRendersFromMeta(t *testing.T) {
	m := templates.AgentMeta{
		BaseImage:  "python:3.11-slim",
		InstallCmd: "pip install aider-chat",
	}
	got := agentDockerfile(m)
	want := "FROM python:3.11-slim\n" +
		"RUN apt-get update && apt-get install -y --no-install-recommends curl git ca-certificates && rm -rf /var/lib/apt/lists/*\n" +
		"RUN pip install aider-chat\n" +
		"WORKDIR /workspace"
	if got != want {
		t.Errorf("agentDockerfile(%+v) =\n%s\nwant\n%s", m, got, want)
	}
}

// TestAgentIDCollisionCoversImageForsSwitch checks agentIDCollision rejects
// every non-agent template id, not just the ones in enrichedDockerfiles.
func TestAgentIDCollisionCoversImageForsSwitch(t *testing.T) {
	for id, img := range nonAgentBaseImages {
		if err := agentIDCollision(id); err == nil {
			t.Errorf("agentIDCollision(%q) = nil, want an error — %q is a non-agent template id/alias resolving to %q via nonAgentBaseImages", id, id, img)
		}
	}
	for id := range templates.AgentMetaByID() {
		if err := agentIDCollision(id); err != nil {
			t.Errorf("agentIDCollision(%q) = %v, want nil — a real agent id must not collide with anything", id, err)
		}
	}
}
