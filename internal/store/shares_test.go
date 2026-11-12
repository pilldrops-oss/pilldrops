package store

import (
	"path/filepath"
	"testing"
)

func shareStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.Context(), filepath.Join(t.TempDir(), "shares.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestShareLifecycle(t *testing.T) {
	st, ctx := shareStore(t), t.Context()
	sh := Share{ID: "sh_1", Name: "quiet-otter-aaaa1111", OrgID: "u:one", CreatedBy: "k1", CreatedAt: 100, ExpiresAt: 200}
	if err := st.InsertShare(ctx, sh); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := st.InsertShare(ctx, Share{ID: "sh_2", Name: sh.Name, OrgID: "u:one", CreatedAt: 100, ExpiresAt: 200}); err == nil {
		t.Fatal("a second share took the same name")
	}
	got, ok, err := st.ShareByName(ctx, sh.Name)
	if err != nil || !ok || got.ID != "sh_1" || got.CreatedBy != "k1" {
		t.Fatalf("by name = %+v %v %v", got, ok, err)
	}
	if s := got.State(150); s != "open" {
		t.Fatalf("state = %q, want open", s)
	}
	if s := got.State(200); s != "expired" {
		t.Fatalf("state = %q, want expired", s)
	}

	if ok, err := st.CloseShare(ctx, "u:two", "sh_1", 150); err != nil || ok {
		t.Fatalf("another org closed it: %v %v", ok, err)
	}
	if ok, err := st.CloseShare(ctx, "u:one", "sh_1", 150); err != nil || !ok {
		t.Fatalf("close: %v %v", ok, err)
	}
	got, _, _ = st.ShareByID(ctx, "sh_1")
	if s := got.State(160); s != "closed" {
		t.Fatalf("state = %q, want closed", s)
	}

	if err := st.AddShareBytes(ctx, "sh_1", 5, 7); err != nil {
		t.Fatalf("bytes: %v", err)
	}
	if err := st.AddShareBytes(ctx, "sh_1", 1, 1); err != nil {
		t.Fatalf("bytes: %v", err)
	}
	got, _, _ = st.ShareByID(ctx, "sh_1")
	if got.BytesIn != 6 || got.BytesOut != 8 {
		t.Fatalf("bytes = %d/%d", got.BytesIn, got.BytesOut)
	}

	blocked, ok, err := st.BlockShare(ctx, sh.Name, 170)
	if err != nil || !ok || blocked.State(171) != "blocked" {
		t.Fatalf("block = %+v %v %v", blocked, ok, err)
	}
}

func TestCountActiveSharesIgnoresDeadOnes(t *testing.T) {
	st, ctx := shareStore(t), t.Context()
	const now, stale = 1000, 880
	for _, sh := range []Share{
		{ID: "a", Name: "a", OrgID: "o", CreatedAt: 990, ExpiresAt: 2000},                  // just made, not yet connected
		{ID: "b", Name: "b", OrgID: "o", CreatedAt: 100, LastSeenAt: 990, ExpiresAt: 2000}, // connected
		{ID: "c", Name: "c", OrgID: "o", CreatedAt: 100, LastSeenAt: 200, ExpiresAt: 2000}, // CLI crashed
		{ID: "d", Name: "d", OrgID: "o", CreatedAt: 100, LastSeenAt: 990, ExpiresAt: 900},  // expired
		{ID: "e", Name: "e", OrgID: "other", CreatedAt: 990, ExpiresAt: 2000},              // another org
	} {
		if err := st.InsertShare(ctx, sh); err != nil {
			t.Fatalf("insert %s: %v", sh.ID, err)
		}
	}
	n, err := st.CountActiveShares(ctx, "o", now, stale)
	if err != nil || n != 2 {
		t.Fatalf("count = %d %v, want 2", n, err)
	}
}

func TestCountConnectedSharesCountsOnlyLiveOthers(t *testing.T) {
	st, ctx := shareStore(t), t.Context()
	const now, stale = 1000, 880
	for _, sh := range []Share{
		{ID: "a", Name: "a", OrgID: "o", CreatedAt: 990, ExpiresAt: 2000},                                 // never connected
		{ID: "b", Name: "b", OrgID: "o", CreatedAt: 100, LastSeenAt: 990, ExpiresAt: 2000},                // connected
		{ID: "c", Name: "c", OrgID: "o", CreatedAt: 100, LastSeenAt: 200, ExpiresAt: 2000},                // CLI crashed
		{ID: "d", Name: "d", OrgID: "o", CreatedAt: 100, LastSeenAt: 990, ExpiresAt: 900},                 // expired
		{ID: "e", Name: "e", OrgID: "o", CreatedAt: 100, LastSeenAt: 990, ExpiresAt: 2000, ClosedAt: 995}, // closed
		{ID: "f", Name: "f", OrgID: "other", CreatedAt: 100, LastSeenAt: 990, ExpiresAt: 2000},            // another org
		{ID: "g", Name: "g", OrgID: "o", CreatedAt: 100, LastSeenAt: 995, ExpiresAt: 2000},                // the one connecting
	} {
		if err := st.InsertShare(ctx, sh); err != nil {
			t.Fatalf("insert %s: %v", sh.ID, err)
		}
	}
	n, err := st.CountConnectedShares(ctx, "o", "g", now, stale)
	if err != nil || n != 1 {
		t.Fatalf("count = %d %v, want 1", n, err)
	}
}
