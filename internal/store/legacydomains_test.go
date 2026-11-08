package store

import (
	"testing"
	"time"
)

func TestLegacyDomainUseNeverMovesBack(t *testing.T) {
	st, ctx := openTemp(t), t.Context()
	if _, ok, err := st.LegacyDomainLastUsed(ctx, "pilldrops.com"); err != nil || ok {
		t.Fatalf("a domain never used: ok=%v err=%v, want neither", ok, err)
	}
	for _, at := range []int64{100, 300, 200} {
		if err := st.TouchLegacyDomain(ctx, "PillDrops.com", at); err != nil {
			t.Fatalf("touch %d: %v", at, err)
		}
	}
	at, ok, err := st.LegacyDomainLastUsed(ctx, "pilldrops.com")
	if err != nil || !ok || at != 300 {
		t.Fatalf("last used = %d ok=%v err=%v, want 300: a late, older write moved the clock back", at, ok, err)
	}
}

func TestDomainLegacyTargetSeen(t *testing.T) {
	st, ctx := openTemp(t), t.Context()
	if err := st.SaveSandbox(ctx, SandboxRecord{
		ID: "sb-1", Template: "base", OrgID: "org1", State: "running", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("save sandbox: %v", err)
	}
	if err := st.PutDomain(ctx, Domain{Host: "app.example.com", OrgID: "org1", SandboxID: "sb-1", Port: 8000, CreatedAt: 10}); err != nil {
		t.Fatalf("put: %v", err)
	}
	seen := func() int64 {
		t.Helper()
		d, ok, err := st.GetDomain(ctx, "app.example.com")
		if err != nil || !ok {
			t.Fatalf("get: ok=%v err=%v", ok, err)
		}
		return d.LegacyTargetSeenAt
	}

	// Traffic on a row the verifier never saw on a legacy target changes nothing.
	if err := st.TouchDomainLegacySeen(ctx, "app.example.com", 500); err != nil {
		t.Fatalf("touch: %v", err)
	}
	if got := seen(); got != 0 {
		t.Fatalf("a row on the current target was stamped %d", got)
	}

	if err := st.SetDomainLegacySeen(ctx, "app.example.com", 100); err != nil {
		t.Fatalf("set: %v", err)
	}
	for _, at := range []int64{500, 400} {
		if err := st.TouchDomainLegacySeen(ctx, "App.Example.com", at); err != nil {
			t.Fatalf("touch %d: %v", at, err)
		}
	}
	if got := seen(); got != 500 {
		t.Fatalf("seen = %d, want 500", got)
	}
	rows, err := st.ListDomainsByOrg(ctx, "org1")
	if err != nil || len(rows) != 1 || rows[0].LegacyTargetSeenAt != 500 {
		t.Fatalf("listed rows = %+v err=%v, want the stamp on the list too", rows, err)
	}

	// A marker older than the window has lapsed, and traffic must not revive it.
	lapsed := int64(500 + LegacyUseWindow/time.Second + 1)
	if err := st.TouchDomainLegacySeen(ctx, "app.example.com", lapsed); err != nil {
		t.Fatalf("touch lapsed: %v", err)
	}
	if got := seen(); got != 500 {
		t.Fatalf("seen = %d after traffic past the window, want 500: a lapsed marker was revived", got)
	}

	if err := st.SetDomainLegacySeen(ctx, "app.example.com", 0); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := seen(); got != 0 {
		t.Fatalf("seen = %d after the row moved to the current target, want 0", got)
	}
}
