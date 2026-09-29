package daemon

import (
	"context"
	"testing"
	"time"
)

// WedgeFS makes FS.Read block until the caller's ctx expires, like a guest that
// has become a TCP black hole; Destroy's log capture must carry its own deadline
// and tolerate its failure.
func TestDestroyIsBoundedWhenGuestCannotAnswer(t *testing.T) {
	old := destroyLogCaptureTimeout
	destroyLogCaptureTimeout = 200 * time.Millisecond
	defer func() { destroyLogCaptureTimeout = old }()

	d, fake := newTestDaemon(t)
	ctx := context.Background()
	sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	fake.WedgeFS(sb.ID)

	done := make(chan error, 1)
	go func() { done <- d.Destroy(ctx, sb.ID) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("destroy of a wedged-guest sandbox must still succeed (logs are forfeit, "+
				"the sandbox is not): %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Destroy did not return within 5s against a guest that cannot answer.\n\n" +
			"The log capture in Destroy must carry its own deadline: without one, a " +
			"fork-bombed or VMM-dead sandbox turns DELETE into a multi-minute hang — a " +
			"tenant-facing DoS on cleanup, since nothing else will ever remove that sandbox.")
	}
	if _, ok := d.Get(sb.ID); ok {
		t.Fatal("sandbox still present after bounded destroy")
	}
}

// Once the sandbox leaves d.boxes the teardown must finish on a detached ctx,
// or a client timeout leaves a removed entry with a live instance.
func TestDestroyCompletesAfterCallerCancels(t *testing.T) {
	d, fake := newTestDaemon(t)
	sb, err := d.Create(context.Background(), CreateSpec{Template: "python-3.11", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the client is already gone when the handler calls Destroy
	if err := d.Destroy(ctx, sb.ID); err != nil {
		t.Fatalf("destroy with a canceled caller ctx: %v", err)
	}
	if !fake.Stopped(sb.ID) {
		t.Fatal("instance was not stopped — a canceled caller aborted the teardown mid-destroy")
	}
}
