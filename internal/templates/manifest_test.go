package templates

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestEveryBuiltinHasLaunchManifest(t *testing.T) {
	for _, tpl := range Catalog() {
		if tpl.Kind == "" {
			t.Errorf("%s: no Kind — add it to launchManifests in manifest.go", tpl.ID)
			continue
		}
		if !slices.Contains(Kinds, tpl.Kind) {
			t.Errorf("%s: Kind %q not in Kinds %v", tpl.ID, tpl.Kind, Kinds)
		}
		if tpl.Outcome == "" {
			t.Errorf("%s: no Outcome", tpl.ID)
		}
		if tpl.PrimaryInput != nil {
			if tpl.PrimaryInput.Type == "" || tpl.PrimaryInput.Label == "" {
				t.Errorf("%s: PrimaryInput needs type+label, got %+v", tpl.ID, tpl.PrimaryInput)
			}
		}
	}
}

func TestAgentTemplatesCarryAgentMeta(t *testing.T) {
	for _, tpl := range Catalog() {
		isAgent := tpl.Kind == KindAgent
		hasMeta := tpl.Agent != nil
		if isAgent != hasMeta {
			t.Errorf("%s: kind=%q but Agent metadata present=%v", tpl.ID, tpl.Kind, hasMeta)
			continue
		}
		if !isAgent {
			continue
		}
		a := tpl.Agent
		if a.CLIBin == "" || a.InstallCmd == "" || a.RunCmd == "" || a.ConfigFile == "" {
			t.Errorf("%s: incomplete AgentMeta: %+v", tpl.ID, a)
		}
		if a.KeyEnv == "" && a.KeyNote == "" {
			t.Errorf("%s: neither KeyEnv nor KeyNote set", tpl.ID)
		}
		switch a.ConfigFormat {
		case "json", "toml", "yaml":
		default:
			t.Errorf("%s: ConfigFormat = %q, want json|toml|yaml", tpl.ID, a.ConfigFormat)
		}
		// Only JSON examples are validated, so an unverified example in another format must be omitted.
		if a.ConfigFormat == "json" && !json.Valid([]byte(a.ConfigExample)) {
			t.Errorf("%s: ConfigExample is not valid JSON", tpl.ID)
		}
	}
}

func TestManifestKindAssignments(t *testing.T) {
	want := map[string]string{
		"base":             KindShell,
		"code-interpreter": KindCode,
		"fastapi":          KindApp,
		"browser":          KindScreen,
		"desktop":          KindScreen,
		"android":          KindScreen,
		"android-headless": KindTest,
		"mobile-test":      KindVerdict,
		"playwright":       KindTest,
		"security":         KindVerdict,
		"loadtest":         KindVerdict,
		"claude":           KindAgent,
		"opencode":         KindAgent,
	}
	byID := map[string]Template{}
	for _, tpl := range Catalog() {
		byID[tpl.ID] = tpl
	}
	for id, kind := range want {
		if got := byID[id].Kind; got != kind {
			t.Errorf("%s: Kind = %q, want %q", id, got, kind)
		}
	}
	for _, id := range []string{"security", "loadtest", "mobile-test"} {
		pi := byID[id].PrimaryInput
		if pi == nil || !pi.Required {
			t.Errorf("%s: verdict template needs a required PrimaryInput, got %+v", id, pi)
		}
	}
	raw, err := json.Marshal(byID["claude"])
	if err != nil {
		t.Fatal(err)
	}
	var back Template
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Kind != KindAgent || back.Agent == nil || back.Agent.KeyEnv != "ANTHROPIC_API_KEY" {
		t.Errorf("claude manifest lost in JSON round-trip: %+v", back)
	}
}
