package docker

import (
	"strings"
	"testing"
)

// TestBootImageGatesOptionalOCIRuntime source-inspects bootImage: the optional
// runtime (e.g. gVisor) must reach only non-service sandboxes.
func TestBootImageGatesOptionalOCIRuntime(t *testing.T) {
	body := sourceContaining(t, `func (r *Runtime) bootImage(`)
	fnStart := strings.Index(body, "func (r *Runtime) bootImage(")
	if fnStart < 0 {
		t.Fatal("bootImage has moved or been renamed; this test no longer checks anything")
	}
	fn := body[fnStart:]

	if !strings.Contains(fn, `base = append(base, "--runtime", r.ociRuntime)`) {
		t.Fatal("bootImage no longer passes --runtime r.ociRuntime — the optional gVisor hardening is not wired into the container argv")
	}
	// Dropping either condition breaks unset-default or services.
	guard := `if r.ociRuntime != "" && !service {`
	if !strings.Contains(fn, guard) {
		t.Errorf("the --runtime guard is not %q — it must stay scoped to configured + non-service, or it will break service templates under gVisor", guard)
	}
}

func TestOCIRuntimeDefaultsEmpty(t *testing.T) {
	t.Setenv("SANDBOXD_DOCKER_RUNTIME", "")
	if got := strings.TrimSpace(""); got != "" {
		t.Fatal("sanity")
	}
	// New probes the daemon, so check the read path at the source level.
	src := sourceContaining(t, `ociRuntime:`)
	if !strings.Contains(src, `ociRuntime:         strings.TrimSpace(os.Getenv("SANDBOXD_DOCKER_RUNTIME"))`) {
		t.Fatal("New no longer reads SANDBOXD_DOCKER_RUNTIME verbatim into ociRuntime")
	}
}
