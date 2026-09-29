package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/runtime/runtimetest"
)

// recordFake adds the single-row lookup a moved-in create reads.
type recordFake struct{ *fakePersister }

func (r recordFake) SandboxRecord(_ context.Context, id string) (PersistedSandbox, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.saved[id]
	return rec, ok, nil
}

func TestMovedInCreateKeepsTheIDAndTheStoredConfig(t *testing.T) {
	fp := recordFake{newFakePersister(nil)}
	const id = "sb-1790000000000-0123abcd"
	fp.saved[id] = PersistedSandbox{ID: id, OrgID: "o", Template: "base", Name: "kept",
		Slug: "kept-slug", CreatedBy: "key_1", Visibility: "private", State: "running", CreatedAt: 100}
	d := New(Config{Runtime: runtimetest.NewFake(), Store: fp, IdleTimeout: time.Minute, HardTTL: time.Hour})
	ctx := context.Background()

	sb, err := d.Create(ctx, CreateSpec{Template: "snap-x", OrgID: "o", ID: id})
	if err != nil {
		t.Fatalf("create under a moved id: %v", err)
	}
	if sb.ID != id || sb.Name != "kept" || sb.Slug != "kept-slug" || sb.CreatedBy != "key_1" || sb.Template != "snap-x" {
		t.Fatalf("moved sandbox = %+v; want the stored name, slug and owner under the same id, on the new template", sb)
	}
	if _, err := d.Create(ctx, CreateSpec{Template: "snap-x", OrgID: "o", ID: id}); !errors.Is(err, ErrBadMoveID) {
		t.Fatalf("a second create under a live id: err = %v, want ErrBadMoveID", err)
	}
	if _, err := d.Create(ctx, CreateSpec{Template: "snap-x", OrgID: "o", ID: "sb-1790000000000-0123abce"}); !errors.Is(err, ErrBadMoveID) {
		t.Fatalf("an id with no record (destroyed mid-move) was created fresh: err = %v", err)
	}
	fp.saved["sb-1790000000000-99999999"] = PersistedSandbox{ID: "sb-1790000000000-99999999", OrgID: "victim"}
	if _, err := d.Create(ctx, CreateSpec{Template: "snap-x", OrgID: "o", ID: "sb-1790000000000-99999999"}); !errors.Is(err, ErrBadMoveID) {
		t.Fatalf("taking another org's id: err = %v, want ErrBadMoveID", err)
	}
	for _, bad := range []string{"../etc", "sb-1-xyz", "sb--0123abcd", "sb-12a-0123abcd"} {
		if _, err := d.Create(ctx, CreateSpec{Template: "snap-x", OrgID: "o", ID: bad}); !errors.Is(err, ErrBadMoveID) {
			t.Errorf("id %q accepted", bad)
		}
	}
}

// Evicting leaves everything keyed by the id to the copy that now runs elsewhere.
func TestEvictKeepsTheRecordAndWritesNoHistory(t *testing.T) {
	fp := newFakePersister(nil)
	d := New(Config{Runtime: runtimetest.NewFake(), Store: fp, IdleTimeout: time.Minute, HardTTL: time.Hour})
	ctx := context.Background()
	sb, err := d.Create(ctx, CreateSpec{Template: "base", OrgID: "o"})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Evict(ctx, sb.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.Get(sb.ID); ok {
		t.Fatal("the evicted sandbox is still on this node")
	}
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if fp.deleted[sb.ID] {
		t.Fatal("evict deleted the row the moved copy now owns")
	}
	if _, ok := fp.history[sb.ID]; ok {
		t.Fatal("evict wrote a history record for a sandbox that did not end")
	}
}
