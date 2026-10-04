package cluster

import (
	"testing"
	"time"
)

// fetchNode builds a firecracker node; pass []string{} for "holds nothing",
// since nil means "boots anything".
func fetchNode(id string, memAvail int, holds []string, canFetch bool) *Node {
	return &Node{
		ID: id, Runtime: "firecracker", Templates: holds, CanFetchTemplates: canFetch,
		Cap:      Capacity{MemAvailMB: memAvail, MemTotalMB: 32000},
		LastSeen: time.Now(),
	}
}

func TestNodeWithASharedStoreIsEligibleForAnyTemplate(t *testing.T) {
	t.Parallel()
	now, ttl := time.Now(), time.Minute

	seeded := fetchNode("seeded", 4000, []string{"base"}, false)
	fresh := fetchNode("fresh", 4000, []string{}, true)

	stuck := fetchNode("stuck", 4000, []string{"other"}, false)
	if got := Pick([]*Node{stuck}, Request{MemMB: 512, Template: "base"}, now, ttl); got != nil {
		t.Fatalf("picked %q, which neither holds nor can fetch the template", got.ID)
	}

	if got := Pick([]*Node{fresh}, Request{MemMB: 512, Template: "base"}, now, ttl); got == nil {
		t.Fatal("a node with a shared artifact store must be eligible for a template it has not fetched yet")
	}
	_ = seeded
}

func TestPickPrefersANodeThatAlreadyHoldsTheTemplate(t *testing.T) {
	t.Parallel()
	now, ttl := time.Now(), time.Minute

	// The fetching node is the tighter fit.
	holds := fetchNode("holds", 8000, []string{"base"}, true)
	fetches := fetchNode("fetches", 2000, []string{}, true)

	got := Pick([]*Node{fetches, holds}, Request{MemMB: 512, Template: "base"}, now, ttl)
	if got == nil || got.ID != "holds" {
		t.Fatalf("picked %v, want the node that already holds the template even though it is a worse fit", got)
	}
}

func TestLocalityOutranksZone(t *testing.T) {
	t.Parallel()
	now, ttl := time.Now(), time.Minute

	inZoneFetches := fetchNode("in-zone", 4000, []string{}, true)
	inZoneFetches.Zone = "eu"
	outZoneHolds := fetchNode("out-zone", 4000, []string{"base"}, true)
	outZoneHolds.Zone = "us"

	got := Pick([]*Node{inZoneFetches, outZoneHolds}, Request{MemMB: 512, Template: "base", Zone: "eu"}, now, ttl)
	if got == nil || got.ID != "out-zone" {
		t.Fatalf("picked %v, want the cross-zone node that holds the template", got)
	}
}

func TestExistingRulesStillApplyWithinATier(t *testing.T) {
	t.Parallel()
	now, ttl := time.Now(), time.Minute

	loose := fetchNode("loose", 16000, []string{"base"}, false)
	tight := fetchNode("tight", 2000, []string{"base"}, false)
	got := Pick([]*Node{loose, tight}, Request{MemMB: 512, Template: "base"}, now, ttl)
	if got == nil || got.ID != "tight" {
		t.Fatalf("picked %v, want the tightest fit among nodes that hold it", got)
	}
}

// An older build reports templates but not can_fetch_templates, which decodes as false.
func TestPreUpgradeNodeIsTreatedAsHoldingOnlyWhatItReports(t *testing.T) {
	t.Parallel()
	now, ttl := time.Now(), time.Minute

	old := fetchNode("old-build", 4000, []string{"base"}, false)

	if got := Pick([]*Node{old}, Request{MemMB: 512, Template: "base"}, now, ttl); got == nil {
		t.Fatal("a pre-upgrade node must still be picked for a template it reports")
	}
	if got := Pick([]*Node{old}, Request{MemMB: 512, Template: "python"}, now, ttl); got != nil {
		t.Fatalf("picked %q for a template it does not hold and cannot fetch", got.ID)
	}
}

func TestUnrestrictedNodeKeepsFullEligibility(t *testing.T) {
	t.Parallel()
	now, ttl := time.Now(), time.Minute

	docker := &Node{
		ID: "docker-1", Runtime: "docker",
		Cap:      Capacity{MemAvailMB: 4000, MemTotalMB: 32000},
		LastSeen: time.Now(),
	}
	if got := Pick([]*Node{docker}, Request{MemMB: 512, Template: "anything"}, now, ttl); got == nil {
		t.Fatal("a node advertising no template list must remain unrestricted")
	}
	if !docker.holdsTemplate("anything") {
		t.Error("an unrestricted node counts as holding everything, so it is in the preferred tier")
	}
}

func TestAFetchNeedsAReadyBuild(t *testing.T) {
	t.Parallel()
	now, ttl := time.Now(), time.Minute
	fresh := fetchNode("fresh", 4000, []string{}, true)
	if got := Pick([]*Node{fresh}, Request{MemMB: 512, Template: "browser", NoBuild: true}, now, ttl); got != nil {
		t.Fatalf("picked %q to fetch a template the store has no build of", got.ID)
	}
}
