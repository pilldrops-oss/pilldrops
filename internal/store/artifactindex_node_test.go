package store

import (
	"errors"
	"testing"
)

func TestReadyBuildIgnoresUnpublishedBuilds(t *testing.T) {
	st := openTemp(t)
	ctx := t.Context()

	if _, err := st.exec(ctx,
		`INSERT INTO template_builds (build_id, template_id, created_at) VALUES (?, ?, ?)`,
		"bld-building", "base", int64(100)); err != nil {
		t.Fatalf("insert building row: %v", err)
	}
	if _, err := st.exec(ctx,
		`INSERT INTO template_builds (build_id, template_id, status, created_at) VALUES (?, ?, 'failed', ?)`,
		"bld-failed", "base", int64(101)); err != nil {
		t.Fatalf("insert failed row: %v", err)
	}

	got, err := st.ReadyBuildForTemplate(ctx, "base")
	if !errors.Is(err, ErrNoReadyBuild) {
		t.Fatalf("resolved %q from a template whose only builds are 'building' and 'failed' "+
			"(err = %v) — the node would fetch a key whose object does not exist yet", got, err)
	}

	markReady(t, st, "bld-ready", "base", 200)
	if got, err := st.ReadyBuildForTemplate(ctx, "base"); err != nil || got != "bld-ready" {
		t.Fatalf("after publishing, got (%q, %v), want bld-ready — the negative above proves "+
			"nothing if the positive never works", got, err)
	}
}

func TestReadyBuildPrefersTheNewestAndIsDeterministic(t *testing.T) {
	st := openTemp(t)
	ctx := t.Context()

	markReady(t, st, "bld-old", "base", 100)
	markReady(t, st, "bld-new", "base", 300)
	markReady(t, st, "bld-mid", "base", 200)

	got, err := st.ReadyBuildForTemplate(ctx, "base")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "bld-new" {
		t.Errorf("resolved %q, want bld-new — a node that boots a superseded build serves "+
			"stale code for a template that was republished", got)
	}

	// Same-second publishes must resolve deterministically.
	markReady(t, st, "bld-tie-a", "tied", 500)
	markReady(t, st, "bld-tie-b", "tied", 500)
	first, err := st.ReadyBuildForTemplate(ctx, "tied")
	if err != nil {
		t.Fatalf("resolve tied: %v", err)
	}
	for i := 0; i < 20; i++ {
		again, err := st.ReadyBuildForTemplate(ctx, "tied")
		if err != nil {
			t.Fatalf("resolve tied (%d): %v", i, err)
		}
		if again != first {
			t.Fatalf("two builds published in the same second resolved to %q then %q — "+
				"the same template id boots different bytes depending on the query plan",
				first, again)
		}
	}
}

func TestReadyBuildIsScopedToItsTemplate(t *testing.T) {
	st := openTemp(t)
	markReady(t, st, "bld-other", "desktop", 900)

	if got, err := st.ReadyBuildForTemplate(t.Context(), "base"); !errors.Is(err, ErrNoReadyBuild) {
		t.Fatalf("asking for 'base' returned (%q, %v) when only 'desktop' is published — "+
			"a node would boot another template's rootfs", got, err)
	}
}

// markReady inserts a ready build directly; the CHECK constraints require a
// size and a 64-char digest.
func markReady(t *testing.T, st *Store, buildID, templateID string, readyAt int64) {
	t.Helper()
	if _, err := st.exec(t.Context(), `
		INSERT INTO template_builds
			(build_id, template_id, status, rootfs_size, rootfs_sha256, created_at, ready_at)
		VALUES (?, ?, 'ready', ?, ?, ?, ?)`,
		buildID, templateID, int64(4096), hex64, readyAt-1, readyAt); err != nil {
		t.Fatalf("publish %s: %v", buildID, err)
	}
}
