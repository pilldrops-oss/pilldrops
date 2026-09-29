package daemon

import (
	"context"
	"errors"
	"testing"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// The runtime must never be reached: both runtimes join the template id into a host path first.
func TestCreateRefusesAPathMovingTemplateBeforeTheRuntimeSeesIt(t *testing.T) {
	for _, name := range []string{"../../../etc", "..", "sub/dir", "/etc/passwd", ""} {
		d, fake := newTestDaemon(t)
		_, err := d.Create(context.Background(), CreateSpec{Template: name, OrgID: "default"})
		if err == nil {
			t.Errorf("Create(template=%q) succeeded", name)
			continue
		}
		if !errors.Is(err, rt.ErrInvalidTemplateName) {
			t.Errorf("Create(template=%q) = %v, want ErrInvalidTemplateName (handleCreate maps that to 400)", name, err)
		}
		if spec, ok := fake.LastBootSpec(); ok {
			t.Errorf("Create(template=%q) reached the runtime with Template=%q — the path join already happened",
				name, spec.Template.Name)
		}
	}
}

// Positive control: a Create that refused everything would leave the test above green.
func TestCreateStillBootsAValidTemplate(t *testing.T) {
	d, fake := newTestDaemon(t)
	if _, err := d.Create(context.Background(), CreateSpec{Template: "code-interpreter", OrgID: "default"}); err != nil {
		t.Fatalf("Create(template=code-interpreter) = %v, want success", err)
	}
	spec, ok := fake.LastBootSpec()
	if !ok {
		t.Fatal("a valid template did not reach the runtime; the guard refuses everything")
	}
	if spec.Template.Name != "code-interpreter" {
		t.Errorf("booted %q, want code-interpreter", spec.Template.Name)
	}
}
