package firecracker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/egress"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/vm"
)

// The restore must be pointed at the directory the node fetched, and must give
// the child its own credential: the memory image still holds the captured
// machine's, and a child answering to it would be the source's twin.
func TestBootFromSnapshotRestoresTheFetchedDirectoryWithANewIdentity(t *testing.T) {
	t.Setenv("FC_MEM_SNAPSHOTS", "1")
	be := newFake()
	r := newWithBackend(be)
	r.netns = true

	rules := &egress.Rules{}
	inst, kind, err := r.BootFromSnapshot(context.Background(),
		rt.SnapshotRef{Path: "/var/lib/sandbox/templates/snap-abc-golden", Name: "snap-abc-golden"},
		rt.BootSpec{SandboxID: "sb-child", VCPU: 2, MemMiB: 2048,
			Identity: rt.Identity{Token: "pd_sb_child"}, Egress: rules})
	if err != nil {
		t.Fatalf("BootFromSnapshot: %v", err)
	}
	if inst == nil {
		t.Fatal("no instance")
	}
	if be.bootedFrom != "/var/lib/sandbox/templates/snap-abc-golden" {
		t.Fatalf("the restore was pointed at %q, not at the fetched build directory", be.bootedFrom)
	}
	if be.bootedEgress != rules {
		t.Error("the child did not get the create's own egress policy")
	}
	if kind != "memory" {
		t.Errorf("kind = %q; a firecracker capture carries RAM and must say so, or the API "+
			"reports a cold boot as a snapshot one", kind)
	}
	if len(be.identities) != 1 || !strings.HasSuffix(be.identities[0], ":pd_sb_child") {
		t.Fatalf("identities delivered = %v; the child must not answer to the captured "+
			"machine's credential", be.identities)
	}
}

// Both gates refuse rather than boot something that cannot work, and each names
// what to change.
func TestBootFromSnapshotRefusesWithoutItsTwoPrerequisites(t *testing.T) {
	be := newFake()

	t.Run("without FC_MEM_SNAPSHOTS", func(t *testing.T) {
		t.Setenv("FC_MEM_SNAPSHOTS", "0")
		r := newWithBackend(be)
		r.netns = true
		if _, _, err := r.BootFromSnapshot(context.Background(),
			rt.SnapshotRef{Path: "/d"}, rt.BootSpec{SandboxID: "x"}); err == nil ||
			!strings.Contains(err.Error(), "FC_MEM_SNAPSHOTS") {
			t.Fatalf("err = %v; must name the setting to change", err)
		}
	})

	t.Run("without per-sandbox namespaces", func(t *testing.T) {
		t.Setenv("FC_MEM_SNAPSHOTS", "1")
		r := newWithBackend(be)
		r.netns = false
		_, _, err := r.BootFromSnapshot(context.Background(),
			rt.SnapshotRef{Path: "/d"}, rt.BootSpec{SandboxID: "x"})
		if !errors.Is(err, vm.ErrForkNeedsNetNS) {
			t.Fatalf("err = %v, want ErrForkNeedsNetNS: the capture's tap name and address are "+
				"baked into the memory image and are already live on the shared bridge", err)
		}
	})

	// Positive control: with both in place the same call succeeds, so the two
	// refusals above are the gates and not a broken fake.
	t.Setenv("FC_MEM_SNAPSHOTS", "1")
	r := newWithBackend(be)
	r.netns = true
	if _, _, err := r.BootFromSnapshot(context.Background(),
		rt.SnapshotRef{Path: "/d"}, rt.BootSpec{SandboxID: "x"}); err != nil {
		t.Fatalf("with both prerequisites the boot still failed: %v", err)
	}
}

// The capability has to be false wherever the boot would be refused, or the
// control plane schedules a create onto a node that cannot serve it.
func TestNamedSnapshotCapabilityMatchesWhatTheBootWillDo(t *testing.T) {
	for _, tc := range []struct {
		name        string
		memSnaps    string
		netns, want bool
	}{
		{"both", "1", true, true},
		{"no netns", "1", false, false},
		{"no memory snapshots", "0", true, false},
		{"neither", "0", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FC_MEM_SNAPSHOTS", tc.memSnaps)
			r := newWithBackend(newFake())
			r.netns = tc.netns
			if got := r.Capabilities().NamedSnapshot; got != tc.want {
				t.Fatalf("NamedSnapshot = %v, want %v — the capability and the boot disagree, "+
					"so a create is either scheduled where it cannot run or refused where it can",
					got, tc.want)
			}
		})
	}
}
