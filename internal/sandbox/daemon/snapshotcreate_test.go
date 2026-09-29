package daemon

import (
	"strings"
	"testing"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// A create naming a snapshot must go through the engine's snapshot path, not
// Boot. The failure this guards is silent: booting the template cold produces a
// running sandbox with the right id and the wrong contents, and every status
// code on the way is 201.
func TestCreateFromASnapshotRestoresInsteadOfBooting(t *testing.T) {
	d, fake := newTestDaemon(t)
	templates.SetDir(t.TempDir())
	t.Cleanup(func() { templates.SetDir("") })

	info, err := d.Create(t.Context(), CreateSpec{
		Template: "snap-abc-golden", Snapshot: "golden", OrgID: "acme",
	})
	if err != nil {
		t.Fatalf("create from snapshot: %v", err)
	}
	boots := fake.SnapshotBoots()
	if len(boots) != 1 {
		t.Fatalf("the engine's snapshot path was called %d times, want once — a cold boot of the "+
			"template answers 201 with the wrong machine", len(boots))
	}
	// Pointed at the fetched template directory, which is where the node's
	// artifact fetch materialises a snapshot's files.
	if !strings.HasSuffix(boots[0], "@"+templates.Dir()+"/snap-abc-golden") {
		t.Errorf("restored from %q, not from the fetched template directory", boots[0])
	}
	if info.BootType != "snapshot" {
		t.Errorf("boot_type = %q, want snapshot: this machine woke out of a memory image", info.BootType)
	}

	// A disk-only engine says so, and the API must not call that a snapshot boot.
	fake.SnapshotBootKind = "disk"
	info, err = d.Create(t.Context(), CreateSpec{
		Template: "snap-abc-golden", Snapshot: "golden", OrgID: "acme",
	})
	if err != nil {
		t.Fatalf("create from a disk snapshot: %v", err)
	}
	if info.BootType != "cold" {
		t.Errorf("boot_type = %q for a disk capture, want cold — its processes are not running "+
			"and the start command still has to run", info.BootType)
	}
}

// Positive control for the test above: an ordinary create must NOT take the
// snapshot path, or the assertion there would pass for the wrong reason.
func TestAnOrdinaryCreateDoesNotTakeTheSnapshotPath(t *testing.T) {
	d, fake := newTestDaemon(t)
	if _, err := d.Create(t.Context(), CreateSpec{Template: "base", OrgID: "acme"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if boots := fake.SnapshotBoots(); len(boots) != 0 {
		t.Fatalf("an ordinary create went through the snapshot path: %v", boots)
	}
}

// A runtime without the capability refuses, naming itself, rather than booting
// the template cold and handing back a machine that is not the snapshot.
func TestCreateFromASnapshotOnARuntimeThatCannotIsRefused(t *testing.T) {
	d, fake := newTestDaemon(t)
	fake.NoSnapshotBoot = true

	_, err := d.Create(t.Context(), CreateSpec{
		Template: "snap-abc-golden", Snapshot: "golden", OrgID: "acme",
	})
	if err == nil {
		t.Fatal("a runtime advertising no named-snapshot support accepted the create")
	}
	if !strings.Contains(err.Error(), rt.ErrUnsupported.Error()) {
		t.Errorf("err = %v; the HTTP layer maps ErrUnsupported to 501", err)
	}
	if boots := fake.SnapshotBoots(); len(boots) != 0 {
		t.Errorf("it was refused and still called the engine: %v", boots)
	}
}
