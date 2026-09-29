package store

import (
	"errors"
	"testing"
)

// The slug column is the platform-wide arbiter of a name: two live rows can
// never hold the same non-empty slug, while any number may hold none.
func TestSandboxSlugIsUniqueAcrossLiveRows(t *testing.T) {
	s := openTemp(t)
	ctx := t.Context()
	row := func(id, slug string) SandboxRecord {
		return SandboxRecord{ID: id, Template: "base", Slug: slug, OrgID: "o1", State: "running", CreatedAt: 1, ExpiresAt: 2, UpdatedAt: 1}
	}
	if err := s.SaveSandbox(ctx, row("a", "demo")); err != nil {
		t.Fatalf("first holder: %v", err)
	}
	err := s.SaveSandbox(ctx, row("b", "demo"))
	if !errors.Is(err, ErrSlugTaken) {
		t.Fatalf("second holder err = %v, want ErrSlugTaken", err)
	}
	// Re-saving the holder itself is not a conflict (every persist goes through the upsert).
	if err := s.SaveSandbox(ctx, row("a", "demo")); err != nil {
		t.Fatalf("holder re-save: %v", err)
	}
	// Many rows with no slug coexist.
	for _, id := range []string{"c", "d", "e"} {
		if err := s.SaveSandbox(ctx, row(id, "")); err != nil {
			t.Fatalf("empty slug %s: %v", id, err)
		}
	}
	got, ok, err := s.SandboxBySlug(ctx, "demo")
	if err != nil || !ok || got.ID != "a" {
		t.Fatalf("SandboxBySlug(demo) = %+v %v %v, want row a", got, ok, err)
	}
	if _, ok, _ := s.SandboxBySlug(ctx, ""); ok {
		t.Fatal("SandboxBySlug(\"\") matched a row; the empty slug is 'none', never a key")
	}
	// Destroy frees the name.
	if err := s.DeleteSandbox(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSandbox(ctx, row("b", "demo")); err != nil {
		t.Fatalf("after destroy, second holder: %v", err)
	}
	recs, err := s.SandboxesByOrg(ctx, "o1")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.ID == "b" && r.Slug != "demo" {
			t.Fatalf("slug not read back: %+v", r)
		}
	}
}
