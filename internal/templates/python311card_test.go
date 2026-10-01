package templates

import "testing"

// python-3.11 is an alias of code-interpreter, so a node that holds its image
// would list it under code-interpreter's name, description and package list,
// which its image (no matplotlib) does not match. Its manifest says what it is.
func TestPython311ManifestIsNotCodeInterpreter(t *testing.T) {
	got := loadManifest("../../templates", "python-3.11")
	ci, _ := builtin("code-interpreter")
	if got.Name == ci.Name || got.Description == ci.Description {
		t.Fatalf("python-3.11 still reads as code-interpreter: %q / %q", got.Name, got.Description)
	}
	for _, p := range got.Preinstalled {
		if p == "matplotlib" {
			t.Fatalf("python-3.11 claims matplotlib, which its image lacks: %v", got.Preinstalled)
		}
	}
}
