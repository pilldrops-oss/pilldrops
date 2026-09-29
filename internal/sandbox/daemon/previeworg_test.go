package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/runtimetest"
)

func previewOrgDaemon(t *testing.T) *Daemon {
	t.Helper()
	d := New(Config{Runtime: runtimetest.NewFake(), IdleTimeout: time.Hour, HardTTL: time.Hour})
	t.Cleanup(func() { d.Shutdown(context.Background()) })
	return d
}

func TestPreviewTargetRefusesAnotherOrgsSandbox(t *testing.T) {
	ctx := context.Background()
	d := previewOrgDaemon(t)
	a, err := d.Create(ctx, CreateSpec{Template: "t", OrgID: "org-a"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Positive control first, or the refusal below could pass for the wrong reason.
	if _, err := d.PreviewTarget(ctx, a.ID, 8000, "org-a"); err != nil {
		t.Fatalf("control: the OWNING org could not resolve its own sandbox (%v) — "+
			"the refusal assertions below would then pass for the wrong reason", err)
	}

	if _, err := d.PreviewTarget(ctx, a.ID, 8000, "org-b"); !errors.Is(err, rt.ErrForeignOrg) {
		t.Fatalf("org-b resolved org-a's sandbox: err = %v, want ErrForeignOrg", err)
	}
}

// "" is what a caller that came through no gate supplies; it must not be a wildcard.
func TestPreviewTargetRefusesAnEmptyOrg(t *testing.T) {
	ctx := context.Background()
	d := previewOrgDaemon(t)
	sb, err := d.Create(ctx, CreateSpec{Template: "t", OrgID: "org-a"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := d.PreviewTarget(ctx, sb.ID, 8000, ""); !errors.Is(err, rt.ErrForeignOrg) {
		t.Fatalf("an empty org resolved a sandbox: err = %v, want ErrForeignOrg", err)
	}
}

// Distinct messages would be an existence oracle for sandbox ids.
func TestForeignOrgRefusalIsIndistinguishableFromNotFound(t *testing.T) {
	if rt.ErrForeignOrg.Error() != rt.ErrNotFound.Error() {
		t.Errorf("ErrForeignOrg = %q, ErrNotFound = %q — a caller that renders err.Error() "+
			"now tells an unauthorized stranger whether a sandbox id exists",
			rt.ErrForeignOrg, rt.ErrNotFound)
	}
	// Still distinct sentinels, or the errors.Is checks in this file are vacuous.
	if errors.Is(rt.ErrForeignOrg, rt.ErrNotFound) {
		t.Error("ErrForeignOrg and ErrNotFound are the same sentinel; errors.Is cannot tell them apart and the assertions in this file are vacuous")
	}
}

// A wake starts billing, so the refusal must come before it.
func TestPreviewTargetChecksOrgBeforeWaking(t *testing.T) {
	ctx := context.Background()
	d := previewOrgDaemon(t)
	sb, err := d.Create(ctx, CreateSpec{Template: "t", OrgID: "org-a"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := d.Pause(ctx, sb.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, err := d.PreviewTarget(ctx, sb.ID, 8000, "org-b"); !errors.Is(err, rt.ErrForeignOrg) {
		t.Fatalf("foreign org: err = %v, want ErrForeignOrg", err)
	}
	got, _ := d.Get(sb.ID)
	if got.State != StatePaused {
		t.Fatalf("a foreign-org preview request woke the sandbox anyway (state = %s) — "+
			"it is refused, and it still costs the owner money", got.State)
	}
	// Control: the owner's request does wake it.
	if _, err := d.PreviewTarget(ctx, sb.ID, 8000, "org-a"); err != nil {
		t.Fatalf("control: the owner could not wake its own sandbox: %v", err)
	}
	if got, _ := d.Get(sb.ID); got.State == StatePaused {
		t.Fatal("control: the owner's request did not wake the sandbox either, so the " +
			"no-wake assertion above proves nothing")
	}
}
