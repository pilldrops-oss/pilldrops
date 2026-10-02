package docker

import (
	"strings"
	"testing"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// TestAdoptRecordsAndRecoversTheRunUser checks both halves: bootImage writes the
// runuser label and Adopt reads it. An unset runUser means uid 0.
func TestAdoptRecordsAndRecoversTheRunUser(t *testing.T) {
	boot := sourceContaining(t, "func (r *Runtime) bootImage(")
	if !strings.Contains(boot, `"--label", "sandboxd.runuser="+runUser`) {
		t.Error("bootImage does not record the run user on the container; Adopt has nothing to recover it from " +
			"and every adopted sandbox falls back to a guess")
	}
	adopt := sourceContaining(t, "func (r *Runtime) Adopt(")
	fns := topLevelFuncs(adopt)
	body, ok := fns["Adopt"]
	if !ok {
		t.Fatal("Adopt not found; this test is not reading the function it names")
	}
	if !strings.Contains(body, "runUser:") {
		t.Error("Adopt builds a dockerInstance without setting runUser. The zero value is \"\", which is uid 0: " +
			"restarting sandboxd undoes the privilege drop for every running sandbox.")
	}
	recover, ok := fns["adoptedRunUser"]
	if !ok {
		t.Fatal("adoptedRunUser not found")
	}
	if !strings.Contains(recover, "sandboxd.runuser") {
		t.Error("adoptedRunUser does not read the label bootImage writes")
	}
	// Pre-label containers must fall back to the non-root user, not "".
	if !strings.Contains(recover, "sandboxRunUser") {
		t.Error("adoptedRunUser's fallback for a container booted before the label existed is not sandboxRunUser; " +
			"the upgrade itself would then be the event that un-drops every live sandbox")
	}
}

// TestLiveAdoptKeepsTheSandboxNonRoot boots a real container, adopts it, and
// checks exec still runs as the same non-root uid.
func TestLiveAdoptKeepsTheSandboxNonRoot(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Skipf("docker runtime unavailable: %v", err)
	}
	ctx := t.Context()
	id := "sb-adopt-" + time.Now().Format("150405")

	inst, err := r.Boot(ctx, rt.BootSpec{
		SandboxID: id,
		Template:  rt.Template{Name: "base", Image: "alpine:3"},
		VCPU:      1, MemMiB: 256,
	})
	if err != nil {
		t.Skipf("boot: %v", err)
	}
	t.Cleanup(func() { _, _, _, _ = dockerOut(ctx, "rm", "-f", "--", inst.(*dockerInstance).cid) })

	uidOf := func(i rt.Instance, what string) string {
		res, err := i.Exec(ctx, rt.ExecSpec{Cmd: "id -u"})
		if err != nil {
			t.Fatalf("%s: exec: %v", what, err)
		}
		return strings.TrimSpace(res.Stdout)
	}

	// Positive control: the fresh boot must be non-root or the comparison is meaningless.
	if u := uidOf(inst, "fresh boot"); u == "0" || u == "" {
		t.Fatalf("a freshly booted sandbox execs as uid %q; P1-b's drop is not in effect and this test cannot measure P2-d's half", u)
	}

	adopted, err := r.Adopt(ctx, id)
	if err != nil {
		t.Fatal("Adopt did not find the container it just booted")
	}
	if u := uidOf(adopted, "adopted"); u == "0" {
		t.Error("an ADOPTED sandbox execs as uid 0 while the same container execs non-root before adoption — " +
			"restarting sandboxd undoes the privilege drop")
	} else if u != uidOf(inst, "fresh boot") {
		t.Errorf("adopted sandbox execs as uid %q, the booted one as %q — the identity is not carried across a restart",
			u, uidOf(inst, "fresh boot"))
	}
}
