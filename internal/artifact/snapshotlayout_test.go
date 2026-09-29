package artifact

import "testing"

// A snapshot build's objects must key and unkey like any other artifact, or
// the cache cannot fetch them and the node cannot find which build they belong to.
func TestSnapshotObjectsRoundTripThroughBuildIDOf(t *testing.T) {
	const build = "bld-0123456789ab"
	names := SnapshotObjects()
	if len(names) != 5 {
		t.Fatalf("a snapshot carries %d objects, want memory, state, disk, the engine's "+
			"capture record and the read-only root image it was taken against", len(names))
	}
	for _, name := range names {
		key, err := Key(build, name)
		if err != nil {
			t.Fatalf("Key(%s, %s): %v", build, name, err)
		}
		if got, ok := BuildIDOf(key); !ok || got != build {
			t.Fatalf("BuildIDOf(%q) = %q, %v; want %q", key, got, ok, build)
		}
		if _, err := HeaderKey(build, name); err != nil {
			t.Fatalf("HeaderKey(%s, %s): %v — the cache refuses an object with no block map", build, name, err)
		}
	}
	// PayloadSnapshot is a marker, never an object name: keying it would write
	// a fourth object nothing reads.
	for _, name := range names {
		if name == PayloadSnapshot {
			t.Fatalf("%q is both the payload marker and an object name", name)
		}
	}
}

func TestSnapshotMetadataSurvivesAnOlderReader(t *testing.T) {
	m := Metadata{
		BuildID: "bld-1", TemplateID: "snap-deadbeef-golden", Payload: PayloadSnapshot,
		RootfsSize: 1, RootfsSHA256: "x",
		Snapshot: &SnapshotMeta{Runtime: "firecracker", Template: "base", VCPU: 2, MemMB: 2048,
			TapName: "tap0", VMIP: "10.128.4.2"},
	}
	raw, err := m.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := ParseMetadata(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.PayloadName() != PayloadSnapshot {
		t.Fatalf("payload round-tripped as %q", got.PayloadName())
	}
	if got.Snapshot == nil || got.Snapshot.VMIP != "10.128.4.2" || got.Snapshot.MemMB != 2048 {
		t.Fatalf("snapshot meta round-tripped as %+v; a restore cannot rebuild the guest's "+
			"address or size without it", got.Snapshot)
	}
	// An ordinary build still carries no snapshot block.
	plain, _ := Metadata{BuildID: "b", TemplateID: "base"}.Marshal()
	if p, _ := ParseMetadata(plain); p.Snapshot != nil {
		t.Fatalf("a rootfs build parsed with a snapshot block: %+v", p.Snapshot)
	}
}

// The root image is linked last; see SnapshotObjects.
func TestSnapshotRootfsIsLinkedLast(t *testing.T) {
	objs := SnapshotObjects()
	if objs[len(objs)-1] != NameRootfs {
		t.Fatalf("SnapshotObjects = %v: the rootfs must be last, or a create can boot a capture missing its memory file", objs)
	}
}
