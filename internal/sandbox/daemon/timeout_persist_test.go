package daemon

import (
	"context"
	"testing"
	"time"

	runtimetest "github.com/pilldrops-oss/pilldrops/internal/runtime/runtimetest"
)

// Promotion to persistent must write through, or a restart restores the stale ExpiresAt and reaps it.
func TestSetTimeoutPersists(t *testing.T) {
	fake := runtimetest.NewFake()
	fp := newFakePersister(nil)
	d := New(Config{Runtime: fake, Store: fp, IdleTimeout: time.Minute, HardTTL: time.Hour})

	ctx := context.Background()
	sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := d.SetTimeout(t.Context(), sb.ID, -1); err != nil {
		t.Fatalf("set timeout: %v", err)
	}
	rec, ok := savedRec(fp, sb.ID)
	if !ok {
		t.Fatalf("SetTimeout did not persist a record")
	}
	if rec.ExpiresAt != 0 {
		t.Fatalf("persistent sandbox persisted ExpiresAt = %d, want 0", rec.ExpiresAt)
	}

	if err := d.SetTimeout(t.Context(), sb.ID, 2*time.Hour); err != nil {
		t.Fatalf("set finite timeout: %v", err)
	}
	rec, _ = savedRec(fp, sb.ID)
	if rec.ExpiresAt == 0 {
		t.Fatalf("finite timeout persisted ExpiresAt = 0, want > 0")
	}
}

func TestRefreshPersists(t *testing.T) {
	fake := runtimetest.NewFake()
	fp := newFakePersister(nil)
	d := New(Config{Runtime: fake, Store: fp, IdleTimeout: time.Minute, HardTTL: time.Hour})

	ctx := context.Background()
	sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := d.Refresh(t.Context(), sb.ID); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	rec, ok := savedRec(fp, sb.ID)
	if !ok {
		t.Fatalf("Refresh did not persist a record")
	}
	if rec.ExpiresAt == 0 {
		t.Fatalf("Refresh persisted ExpiresAt = 0 for a finite-TTL sandbox")
	}

	// Refresh on a persistent sandbox must stay a no-op.
	if err := d.SetTimeout(t.Context(), sb.ID, -1); err != nil {
		t.Fatalf("set timeout: %v", err)
	}
	if err := d.Refresh(t.Context(), sb.ID); err != nil {
		t.Fatalf("refresh persistent: %v", err)
	}
	rec, _ = savedRec(fp, sb.ID)
	if rec.ExpiresAt != 0 {
		t.Fatalf("Refresh re-stamped a persistent sandbox: ExpiresAt = %d", rec.ExpiresAt)
	}
}

func savedRec(fp *fakePersister, id string) (PersistedSandbox, bool) {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	rec, ok := fp.saved[id]
	return rec, ok
}
