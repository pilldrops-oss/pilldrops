package templates

import (
	"testing"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// daemon.Create enforces ValidateTemplateName, so tightening it must not refuse
// a shipped id. Lives here because internal/runtime must not import the catalogue.
func TestEveryCatalogueTemplateIDIsValid(t *testing.T) {
	cat := Catalog()
	if len(cat) < 9 {
		t.Fatalf("Catalog() returned %d templates; this test is not seeing the catalogue", len(cat))
	}
	for _, tmpl := range cat {
		if err := rt.ValidateTemplateName(tmpl.ID); err != nil {
			t.Errorf("catalogue template %q is refused by rt.ValidateTemplateName: %v — "+
				"daemon.Create enforces that rule, so this template cannot boot", tmpl.ID, err)
		}
	}
}
