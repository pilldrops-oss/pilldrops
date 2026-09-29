package daemon

import (
	"context"
	"errors"
	"testing"
)

func TestCreateRefusedWhenIsolationPreflightFailed(t *testing.T) {
	d, fake := newTestDaemon(t)
	d.SetIsolationOK(false)

	sb, err := d.Create(context.Background(), CreateSpec{Template: "python-3.11", OrgID: "default"})
	if !errors.Is(err, ErrIsolationPreflight) {
		t.Fatalf("err = %v, want ErrIsolationPreflight", err)
	}
	if sb.ID != "" {
		t.Fatalf("got a non-empty SandboxInfo on refusal: %+v", sb)
	}
	if _, booted := fake.LastBootSpec(); booted {
		t.Fatal("runtime.Boot was called — a refused create must never reach the runtime")
	}
	if len(d.List("")) != 0 {
		t.Fatalf("a sandbox was left behind after a refused create")
	}
}

// Resume is reached from the unauthenticated preview wake path, so the guard must live in Resume.
func TestResumeRefusedWhenIsolationPreflightFailed(t *testing.T) {
	d, _ := newTestDaemon(t)
	ctx := context.Background()

	sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := d.Pause(ctx, sb.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}

	// Preflight fails after the sandbox was created and paused, as on a restart into a bad host.
	d.SetIsolationOK(false)

	if err := d.Resume(ctx, sb.ID); !errors.Is(err, ErrIsolationPreflight) {
		t.Fatalf("resume err = %v, want ErrIsolationPreflight", err)
	}
	if got, _ := d.Get(sb.ID); got.State != StatePaused {
		t.Fatalf("state = %s, want still paused (the refusal must not have woken it)", got.State)
	}
}

func TestCreateAndResumeUnaffectedByDefault(t *testing.T) {
	d, _ := newTestDaemon(t)
	ctx := context.Background()

	sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
	if err != nil {
		t.Fatalf("create with default isolationOK: %v", err)
	}
	if err := d.Pause(ctx, sb.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := d.Resume(ctx, sb.ID); err != nil {
		t.Fatalf("resume with default isolationOK: %v", err)
	}
	if got, _ := d.Get(sb.ID); got.State != StateRunning {
		t.Fatalf("state = %s, want running", got.State)
	}
}
