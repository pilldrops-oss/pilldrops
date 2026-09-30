package daemon

import (
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

func TestAgentTemplateIDOnlyMatchesAgentKind(t *testing.T) {
	for _, tmpl := range templates.Catalog() {
		got := agentTemplateID(tmpl.ID)
		if tmpl.Kind == templates.KindAgent && tmpl.Agent != nil {
			if got != tmpl.ID {
				t.Errorf("%s is kind=agent but agentTemplateID gave %q", tmpl.ID, got)
			}
			continue
		}
		if got != "" {
			t.Errorf("%s is kind=%q but was treated as an agent (%q)", tmpl.ID, tmpl.Kind, got)
		}
	}
}

func TestUnknownTemplateIsNotAnAgent(t *testing.T) {
	if got := agentTemplateID("no-such-template"); got != "" {
		t.Errorf("unknown template gave %q, want empty", got)
	}
}

func TestAgentTemplateIDResolvesAliases(t *testing.T) {
	for _, tmpl := range templates.Catalog() {
		if tmpl.Kind != templates.KindAgent {
			continue
		}
		if got := agentTemplateID(tmpl.ID); got != tmpl.ID {
			t.Errorf("canonical id %s gave %q", tmpl.ID, got)
		}
	}
}
