package store

import (
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
)

// These drive the CHECK constraints behind "a build is ready only after its
// artifact is published and hashed" with raw SQL, on both dialects.

// hex64 is a well-formed SHA-256.
const hex64 = "0000000000000000000000000000000000000000000000000000000000000000"

func TestBuildCannotBeReadyUntilItsArtifactIsPublished(t *testing.T) {
	st := openTemp(t)
	ctx := t.Context()

	// 'building' is the default status.
	if _, err := st.exec(ctx,
		`INSERT INTO template_builds (build_id, template_id, created_at) VALUES (?, ?, ?)`,
		"bld-ready-1", "base", int64(100),
	); err != nil {
		t.Fatalf("insert building row: %v", err)
	}
	var status string
	var readyAt int64
	if err := st.queryRow(ctx,
		`SELECT status, ready_at FROM template_builds WHERE build_id = ?`, "bld-ready-1",
	).Scan(&status, &readyAt); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if status != "building" || readyAt != 0 {
		t.Fatalf("a new build is status=%q ready_at=%d, want building/0 — a build must not be born ready", status, readyAt)
	}

	// Ready without size and checksum must be refused.
	if _, err := st.exec(ctx,
		`UPDATE template_builds SET status = 'ready', ready_at = ? WHERE build_id = ?`,
		int64(200), "bld-ready-1",
	); err == nil {
		t.Error("marked a build ready with no size and no checksum — the ordering invariant is not " +
			"enforced, so an interrupted publish can leave a ready row whose bytes were never written")
	}

	if _, err := st.exec(ctx,
		`UPDATE template_builds SET status = 'ready', ready_at = ?, rootfs_size = ?, rootfs_sha256 = ? WHERE build_id = ?`,
		int64(200), int64(4096), "not-a-real-digest", "bld-ready-1",
	); err == nil {
		t.Error("accepted a 17-character 'checksum' — the constraint is checking presence, not shape")
	}

	if _, err := st.exec(ctx,
		`UPDATE template_builds SET status = 'ready', ready_at = ?, rootfs_size = 0, rootfs_sha256 = ? WHERE build_id = ?`,
		int64(200), hex64, "bld-ready-1",
	); err == nil {
		t.Error("accepted a ready build of size 0")
	}

	if _, err := st.exec(ctx,
		`UPDATE template_builds SET status = 'ready', ready_at = ?, rootfs_size = ?, rootfs_sha256 = ? WHERE build_id = ?`,
		int64(200), int64(4096), hex64, "bld-ready-1",
	); err != nil {
		t.Fatalf("a fully published build was refused, so the constraint is too strict to use: %v", err)
	}
}

func TestBuildCannotBeInsertedAlreadyReady(t *testing.T) {
	st := openTemp(t)

	if _, err := st.exec(t.Context(),
		`INSERT INTO template_builds (build_id, template_id, status, created_at, ready_at) VALUES (?, ?, 'ready', ?, ?)`,
		"bld-insert-ready", "base", int64(100), int64(100),
	); err == nil {
		t.Error("inserted a ready build with no published artifact — the invariant is enforceable on " +
			"UPDATE only, which a single-statement publisher would bypass")
	}
}

func TestReadyTimestampAndStatusCannotDisagree(t *testing.T) {
	st := openTemp(t)
	ctx := t.Context()

	if _, err := st.exec(ctx,
		`INSERT INTO template_builds (build_id, template_id, created_at) VALUES (?, ?, ?)`,
		"bld-skew", "base", int64(100),
	); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := st.exec(ctx,
		`UPDATE template_builds SET ready_at = ? WHERE build_id = ?`, int64(200), "bld-skew",
	); err == nil {
		t.Error("set ready_at on a build still marked 'building' — a query filtering on ready_at > 0 " +
			"and one filtering on status = 'ready' would return different builds")
	}
}

func TestBuildStatusVocabularyIsClosed(t *testing.T) {
	st := openTemp(t)

	if _, err := st.exec(t.Context(),
		`INSERT INTO template_builds (build_id, template_id, status, created_at) VALUES (?, ?, 'published', ?)`,
		"bld-vocab", "base", int64(100),
	); err == nil {
		t.Error("accepted status 'published' — a status no constraint mentions is a build outside the " +
			"readiness rule; adding one must be a migration")
	}
	for _, ok := range []string{"building", "ready", "failed"} {
		id := "bld-vocab-" + ok
		size, sum, ready := int64(0), "", int64(0)
		if ok == "ready" {
			size, sum, ready = 1, hex64, 200
		}
		if _, err := st.exec(t.Context(),
			`INSERT INTO template_builds (build_id, template_id, status, rootfs_size, rootfs_sha256, created_at, ready_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id, "base", ok, size, sum, int64(100), ready,
		); err != nil {
			t.Errorf("status %q is part of the vocabulary but was refused: %v", ok, err)
		}
	}
}

func TestArtifactRowCannotRecordUnverifiedBytes(t *testing.T) {
	st := openTemp(t)
	ctx := t.Context()

	key, err := artifact.Key("bld-art-1", artifact.NameRootfs)
	if err != nil {
		t.Fatalf("artifact.Key: %v", err)
	}

	if _, err := st.exec(ctx,
		`INSERT INTO artifacts (build_id, kind, key, size, store_kind, uploaded_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"bld-art-1", artifact.NameRootfs, key, int64(4096), string(artifact.KindFS), int64(300),
	); err == nil {
		t.Error("inserted an artifact row with no sha256 — a row can be created before the bytes are " +
			"hashed, which is the placeholder this table exists to make impossible")
	}

	if _, err := st.exec(ctx,
		`INSERT INTO artifacts (build_id, kind, key, size, sha256, uploaded_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"bld-art-1", artifact.NameRootfs, key, int64(4096), hex64, int64(300),
	); err == nil {
		t.Error("inserted an artifact row that does not say which store holds the bytes")
	}

	if _, err := st.exec(ctx,
		`INSERT INTO artifacts (build_id, kind, key, size, sha256, store_kind, uploaded_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"bld-art-1", artifact.NameRootfs, key, int64(4096), "deadbeef", string(artifact.KindFS), int64(300),
	); err == nil {
		t.Error("accepted an 8-character sha256")
	}

	if _, err := st.exec(ctx,
		`INSERT INTO artifacts (build_id, kind, key, size, sha256, store_kind, uploaded_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"bld-art-1", artifact.NameRootfs, key, int64(4096), hex64, string(artifact.KindFS), int64(300),
	); err != nil {
		t.Fatalf("a published artifact was refused, so the constraints are too strict to use: %v", err)
	}

	// PRIMARY KEY (build_id, kind).
	if _, err := st.exec(ctx,
		`INSERT INTO artifacts (build_id, kind, key, size, sha256, store_kind, uploaded_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"bld-art-1", artifact.NameRootfs, key, int64(8192), hex64, string(artifact.KindFS), int64(400),
	); err == nil {
		t.Error("two rows for the same build's rootfs — a reader has no way to tell which describes " +
			"the bytes actually at that key")
	}
}

// The key column must be exactly what artifact.Key() returns, so a node can
// compute it from a build id without reading this table.
func TestArtifactKeyColumnMatchesArtifactKeyFunc(t *testing.T) {
	st := openTemp(t)
	ctx := t.Context()

	rootfs, err := artifact.Key("bld-key", artifact.NameRootfs)
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	header, err := artifact.HeaderKey("bld-key", artifact.NameRootfs)
	if err != nil {
		t.Fatalf("HeaderKey: %v", err)
	}
	meta, err := artifact.Key("bld-key", artifact.NameMetadata)
	if err != nil {
		t.Fatalf("Key: %v", err)
	}

	for _, k := range []string{rootfs, header, meta} {
		buildID, ok := artifact.BuildIDOf(k)
		if !ok {
			t.Fatalf("BuildIDOf(%q) failed", k)
		}
		kind := strings.TrimPrefix(k, buildID+"/")
		if _, err := st.exec(ctx,
			`INSERT INTO artifacts (build_id, kind, key, size, sha256, store_kind, uploaded_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			buildID, kind, k, int64(16), hex64, string(artifact.KindControlPlane), int64(300),
		); err != nil {
			t.Errorf("the schema refused the key artifact.Key() produces (%q): %v", k, err)
		}
	}

	// A key that does not derive from its own build id is refused.
	if _, err := st.exec(ctx,
		`INSERT INTO artifacts (build_id, kind, key, size, sha256, store_kind, uploaded_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"bld-key", "memfile", "some-other-build/memfile", int64(16), hex64, string(artifact.KindFS), int64(300),
	); err == nil {
		t.Error("recorded a key belonging to another build — a node computing the key from the build id " +
			"and a reader trusting this column would fetch different objects")
	}

	// ValidateBuildID and the schema must agree on the length limit.
	long := strings.Repeat("a", 129)
	if err := artifact.ValidateBuildID(long); err == nil {
		t.Fatal("ValidateBuildID accepts a 129-byte id; this test's premise is stale")
	}
	if _, err := st.exec(ctx,
		`INSERT INTO template_builds (build_id, template_id, created_at) VALUES (?, ?, ?)`,
		long, "base", int64(100),
	); err == nil {
		t.Error("stored a build id longer than ValidateBuildID accepts")
	}
}
