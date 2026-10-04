package templates

import (
	"slices"
	"testing"
)

// B18: a node holding the python-3.11 image, with no template.json beside it,
// showed code-interpreter's card, promising matplotlib the image lacks.
func TestAnAliasImageDoesNotWearItsCanonicalCard(t *testing.T) {
	dir := t.TempDir()
	publish(t, dir, "python-3.11", "")
	got := loadManifest(dir, "python-3.11")
	if got.Name != "Python 3.11" {
		t.Errorf("name = %q, want Python 3.11", got.Name)
	}
	if slices.Contains(got.Preinstalled, "matplotlib") {
		t.Errorf("preinstalled = %v, which the python-3.11 image does not have", got.Preinstalled)
	}
	canonical, _ := builtin("code-interpreter")
	if got.Description == canonical.Description {
		t.Errorf("description is code-interpreter's: %q", got.Description)
	}
	// A manifest still wins over the compiled-in card.
	publish(t, dir, "python-3.11", `{"name":"Py"}`)
	if got := loadManifest(dir, "python-3.11"); got.Name != "Py" {
		t.Errorf("manifest name = %q, want Py", got.Name)
	}
}
