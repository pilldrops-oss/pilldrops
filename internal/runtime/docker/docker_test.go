package docker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/config"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// A production node refuses an unenforceable disk cap; anything else keeps the
// uncapped fallback. boot.go asks productionNode directly, so this pins what it
// asks -- the previous version of this test compared a parameter to itself.
func TestProductionNodeDecidesTheUncappedFallback(t *testing.T) {
	config.Reset()
	t.Setenv("SANDBOXD_PRODUCTION", "1")
	if !productionNode() {
		t.Fatal("SANDBOXD_PRODUCTION=1 must mark the node production, or an unenforceable disk cap boots uncapped")
	}
	config.Reset()
	t.Setenv("SANDBOXD_PRODUCTION", "")
	if productionNode() {
		t.Fatal("unset must leave the dev/local fallback in place")
	}
}

// TestBoot_RejectsTenantImageOutsideAllowlist checks the allowlist runs before
// any daemon interaction, so no daemon is needed here.
func TestBoot_RejectsTenantImageOutsideAllowlist(t *testing.T) {
	r := &Runtime{imageAllowlist: []string{"only-allowed.example"}}
	_, err := r.Boot(context.Background(), rt.BootSpec{
		Template: rt.Template{Image: "docker.io/evil/image:latest"},
	})
	if err == nil {
		t.Fatal("expected Boot to reject a tenant image outside the allowlist")
	}
	if !strings.Contains(err.Error(), "not in allowlist") {
		t.Fatalf("expected the allowlist-rejection error, got: %v", err)
	}
}

// TestEnsureImage_NeverAllowlistChecked: the allowlist is scoped to tenant
// images, and ensureImage also pulls platform templates, so it must not gate.
func TestEnsureImage_NeverAllowlistChecked(t *testing.T) {
	r := &Runtime{imageAllowlist: []string{"only-allowed.example"}}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := r.ensureImage(ctx, "docker.io/library/definitely-not-a-real-image-xyz123:latest")
	if err == nil {
		t.Skip("image inspect unexpectedly succeeded in this environment; cannot exercise the pull path")
	}
	if strings.Contains(err.Error(), "not in allowlist") {
		t.Fatalf("ensureImage must never run the M4 allowlist backstop (F3 review fix): got %v", err)
	}
}

// TestBoot_PlatformTemplateNeverAllowlistChecked: the short context keeps this
// daemon-independent; only the kind of failure is asserted.
func TestBoot_PlatformTemplateNeverAllowlistChecked(t *testing.T) {
	r := &Runtime{imageAllowlist: []string{"only-allowed.example"}}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := r.Boot(ctx, rt.BootSpec{
		Template:  rt.Template{Name: "python-3.11"},
		SandboxID: liveSandboxID(t, "test-sb-platform-template"),
	})
	if err == nil {
		t.Skip("Boot unexpectedly completed within the timeout in this environment")
	}
	if strings.Contains(err.Error(), "not in allowlist") {
		t.Fatalf("platform template pull must never be allowlist-checked (F3 review fix): got %v", err)
	}
}

// TestEnvDigestRequireFlagAgreesWithTheHTTPLayer computes the expectation from
// config.Bool rather than a table, so the two layers cannot drift unnoticed.
func TestEnvDigestRequireFlagAgreesWithTheHTTPLayer(t *testing.T) {
	const key = "PILLDROPS_IMAGE_REQUIRE_DIGEST"
	inputs := []string{"", "1", "01", "0", "2", "true", "TRUE", "True", "yes",
		"on", "y", "false", "no", "off", " 1", "1 ", " true ", "enabled", "-1"}

	for _, in := range inputs {
		t.Setenv(key, in)
		want := config.Bool(key, false)
		if got := envDigestRequireFlag(in); got != want {
			t.Errorf("PILLDROPS_IMAGE_REQUIRE_DIGEST=%q: the docker backstop resolves %v but "+
				"internal/httpapi resolves %v. One variable, two answers: the create is refused at "+
				"one layer and permitted at the other.", in, got, want)
		}
	}

	// Vacuity guard: both readers returning a constant would pass the loop above.
	if !envDigestRequireFlag("1") {
		t.Error(`envDigestRequireFlag("1") is false; the flag can never be turned on`)
	}
	if envDigestRequireFlag("0") {
		t.Error(`envDigestRequireFlag("0") is true; the flag can never be turned off`)
	}
	if !envDigestRequireFlag("true") {
		t.Error(`envDigestRequireFlag("true") is false — this is the exact input the drift was found on`)
	}
}

// Memory rounds up to whole MiB: a container using less than 1 MiB is
// running, and reporting 0 would read as "uses no memory".
func TestMemUsedMiBRoundsUp(t *testing.T) {
	var s statsJSON
	for _, c := range []struct {
		usage, inactive uint64
		want            int
	}{{700 << 10, 0, 1}, {1 << 20, 0, 1}, {(1 << 20) + 1, 0, 2}, {10 << 20, 4 << 20, 6}, {0, 0, 0}} {
		s.MemoryStats.Usage = c.usage
		s.MemoryStats.Stats = map[string]uint64{"inactive_file": c.inactive}
		if got := memUsedMiB(s); got != c.want {
			t.Errorf("usage %d, inactive %d: %d MiB, want %d", c.usage, c.inactive, got, c.want)
		}
	}
}
