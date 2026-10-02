package vm

import (
	"errors"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/envdproto"
)

func TestGoldenEnvdCheck(t *testing.T) {
	if goldenEnvdCheck(1) == nil {
		t.Error("a level-1 envd cannot bind in a golden VM; the build must be refused before it boots")
	}
	if err := goldenEnvdCheck(envdproto.VsockCredential); err != nil {
		t.Errorf("a vsock-credential envd must be allowed: %v", err)
	}
}

func TestGoldenBuildsNeverRunTwiceAndBackOff(t *testing.T) {
	g := &goldenBuilds{state: map[string]goldenBuildState{}}
	t0 := time.Unix(1_000_000, 0)
	mod := time.Unix(500, 0)

	if !g.start("base", mod, t0) {
		t.Fatal("the first build must start")
	}
	if g.start("base", mod, t0) {
		t.Fatal("a second build of the same template started while the first ran")
	}
	if !g.start("other", mod, t0) {
		t.Fatal("a build of a different template must not wait on this one")
	}

	g.finish("base", errors.New("boot failed"), false, t0)
	if g.start("base", mod, t0.Add(time.Minute)) {
		t.Fatal("a failed build was retried within the back-off window")
	}
	if !g.start("base", mod, t0.Add(goldenRetryAfter+time.Second)) {
		t.Fatal("a failed build was never retried after the window")
	}
	g.finish("base", nil, false, t0)
	if !g.start("base", mod, t0) {
		t.Fatal("a successful build must not hold off the next one")
	}
}

func TestGoldenRefusalHoldsUntilTheTemplateIsRebuilt(t *testing.T) {
	g := &goldenBuilds{state: map[string]goldenBuildState{}}
	t0 := time.Unix(1_000_000, 0)
	oldMod, newMod := time.Unix(500, 0), time.Unix(900, 0)

	g.start("base", oldMod, t0)
	g.finish("base", errors.New("stale envd"), true, t0)
	if g.start("base", oldMod, t0.AddDate(1, 0, 0)) {
		t.Fatal("a refused template was retried without being rebuilt")
	}
	if !g.start("base", newMod, t0) {
		t.Fatal("a rebuilt template (new rootfs) must be retried at once")
	}
}

func TestGoldenVMIDIsJailerLegal(t *testing.T) {
	id := goldenVMID("python-3.11", "ab12cd34")
	if id != "golden-python-3-11-ab12cd34" {
		t.Fatalf("golden id = %q; the jailer rejects any char but letters, digits and '-'", id)
	}
}
