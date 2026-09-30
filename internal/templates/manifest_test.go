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

func TestManifestKindAssignments(t *testing.T) {
	want := map[string]string{
		"base":             KindShell,
		"docker":           KindShell,
		"code-interpreter": KindCode,
		"browser":          KindScreen,
		"desktop":          KindScreen,
		"playwright":       KindTest,
		"node":             KindApp,
		"go":               KindApp,
		"rust":             KindApp,
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
	raw, err := json.Marshal(byID["code-interpreter"])
	if err != nil {
		t.Fatal(err)
	}
	var back Template
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Kind != KindCode || back.PrimaryInput == nil || back.PrimaryInput.Type != "code" {
		t.Errorf("code-interpreter manifest lost in JSON round-trip: %+v", back)
	}
}
