package cluster

import (
	"encoding/json"
	"testing"
	"time"
)

func node(id string, total, avail, committed int, load float64, seen time.Time) *Node {
	return &Node{ID: id, URL: "http://" + id, LastSeen: seen,
		Cap: Capacity{MemTotalMB: total, MemAvailMB: avail, MemCommittedMB: committed, Load1: load}}
}

func runtimeNode(id, runtime string, total, avail, committed int, seen time.Time) *Node {
	n := node(id, total, avail, committed, 1, seen)
	n.Runtime = runtime
	n.Isolation = runtime != "process"
	return n
}

func TestPickFiltersByRuntime(t *testing.T) {
	now := time.Now()
	ttl := 30 * time.Second
	// The docker node is the tighter fit, so only the runtime pin sends this to kvm.
	fc := runtimeNode("kvm", "firecracker", 64000, 40000, 20000, now)
	dk := runtimeNode("box", "docker", 8000, 3000, 5000, now)
	got := Pick([]*Node{fc, dk}, Request{MemMB: 1024, Runtime: "firecracker"}, now, ttl)
	if got == nil || got.ID != "kvm" {
		t.Fatalf("Pick(runtime=firecracker) = %v, want kvm", got)
	}
	if got := Pick([]*Node{fc, dk}, Request{MemMB: 1024}, now, ttl); got == nil || got.ID != "box" {
		t.Fatalf("Pick(any runtime) = %v, want box (best-fit)", got)
	}
}

func TestPickNoMatchingRuntimeReturnsNil(t *testing.T) {
	now := time.Now()
	ttl := 30 * time.Second
	dk := runtimeNode("box", "docker", 8000, 4000, 2000, now)
	if got := Pick([]*Node{dk}, Request{MemMB: 1024, Runtime: "firecracker"}, now, ttl); got != nil {
		t.Fatalf("Pick(runtime=firecracker) with only docker nodes = %v, want nil", got)
	}
}

func TestPickBestFit(t *testing.T) {
	now := time.Now()
	ttl := 30 * time.Second
	nodes := []*Node{
		node("big", 64000, 40000, 20000, 1, now),
		node("tight", 8000, 3000, 5000, 1, now),
		node("full", 8000, 400, 7600, 1, now),
	}
	got := Pick(nodes, Request{VCPU: 1, MemMB: 1024}, now, ttl)
	if got == nil || got.ID != "tight" {
		t.Fatalf("Pick = %v, want tight (best-fit)", got)
	}
}

func TestPickSkipsUnhealthy(t *testing.T) {
	now := time.Now()
	ttl := 30 * time.Second
	nodes := []*Node{
		node("stale", 8000, 5000, 1000, 1, now.Add(-time.Minute)),
		node("live", 64000, 40000, 1000, 1, now),
	}
	got := Pick(nodes, Request{MemMB: 1024}, now, ttl)
	if got == nil || got.ID != "live" {
		t.Fatalf("Pick = %v, want live (stale skipped)", got)
	}
}

func TestPickRespectsOvercommitAndMargin(t *testing.T) {
	now := time.Now()
	ttl := 30 * time.Second
	over := node("over", 8000, 6000, 16000, 1, now)
	if over.fits(Request{MemMB: 1024}) {
		t.Fatal("expected over-committed node to be rejected")
	}
	nearFull := node("nf", 8000, 1200, 2000, 1, now) // 1200 < 1024+512
	if nearFull.fits(Request{MemMB: 1024}) {
		t.Fatal("expected near-full node to be rejected by margin")
	}
	if got := Pick([]*Node{over, nearFull}, Request{MemMB: 1024}, now, ttl); got != nil {
		t.Fatalf("Pick = %v, want nil (nothing fits)", got)
	}
}

func TestPickTieBreaksOnLoad(t *testing.T) {
	now := time.Now()
	ttl := 30 * time.Second
	a := node("a", 16000, 5000, 1000, 3.0, now)
	b := node("b", 16000, 5000, 1000, 0.5, now)
	got := Pick([]*Node{a, b}, Request{MemMB: 1024}, now, ttl)
	if got == nil || got.ID != "b" {
		t.Fatalf("Pick = %v, want b (lower load tiebreak)", got)
	}
}

func TestPickSkipsCordonedAndDraining(t *testing.T) {
	now := time.Now()
	ttl := 30 * time.Second
	cordoned := node("cordoned", 8000, 3000, 0, 1, now)
	cordoned.Cordoned = true
	draining := node("draining", 8000, 4000, 0, 1, now)
	draining.Draining = true
	open := node("open", 64000, 40000, 0, 1, now)
	got := Pick([]*Node{cordoned, draining, open}, Request{MemMB: 1024}, now, ttl)
	if got == nil || got.ID != "open" {
		t.Fatalf("Pick = %v, want open (cordoned+draining skipped)", got)
	}
}

func TestPickPrefersRequestedZone(t *testing.T) {
	now := time.Now()
	ttl := 30 * time.Second
	// The cross-zone node is the tighter fit.
	inZone := node("in", 64000, 40000, 0, 1, now)
	inZone.Zone = "fsn1"
	crossTight := node("cross", 8000, 3000, 0, 1, now)
	crossTight.Zone = "nbg1"
	got := Pick([]*Node{crossTight, inZone}, Request{MemMB: 1024, Zone: "fsn1"}, now, ttl)
	if got == nil || got.ID != "in" {
		t.Fatalf("Pick = %v, want in (requested zone preferred over tighter cross-zone)", got)
	}
}

func TestPickFallsBackAcrossZones(t *testing.T) {
	now := time.Now()
	ttl := 30 * time.Second
	inZoneFull := node("in", 8000, 200, 0, 1, now)
	inZoneFull.Zone = "fsn1"
	other := node("other", 64000, 40000, 0, 1, now)
	other.Zone = "nbg1"
	got := Pick([]*Node{inZoneFull, other}, Request{MemMB: 1024, Zone: "fsn1"}, now, ttl)
	if got == nil || got.ID != "other" {
		t.Fatalf("Pick = %v, want other (fallback across zones)", got)
	}
}

func TestNodeStatus(t *testing.T) {
	now := time.Now()
	ttl := 30 * time.Second

	stale := node("stale", 8000, 5000, 0, 1, now.Add(-time.Minute))
	if got := stale.Status(now, ttl); got != "unreachable" {
		t.Fatalf("stale status = %q, want unreachable", got)
	}

	draining := node("drain", 8000, 5000, 0, 1, now)
	draining.Draining = true
	if got := draining.Status(now, ttl); got != "draining" {
		t.Fatalf("draining status = %q, want draining", got)
	}

	cordoned := node("cord", 8000, 5000, 0, 1, now)
	cordoned.Cordoned = true
	if got := cordoned.Status(now, ttl); got != "cordoned" {
		t.Fatalf("cordoned status = %q, want cordoned", got)
	}

	full := node("full", 8000, 400, 7600, 1, now)
	if got := full.Status(now, ttl); got != "full" {
		t.Fatalf("full status = %q, want full", got)
	}

	open := node("open", 64000, 40000, 0, 1, now)
	if got := open.Status(now, ttl); got != "available" {
		t.Fatalf("open status = %q, want available", got)
	}
}

func TestPickFiltersByTemplate(t *testing.T) {
	now := time.Now()
	ttl := 30 * time.Second
	fc := runtimeNode("kvm", "firecracker", 64000, 40000, 20000, now)
	fc.Templates = []string{"code-interpreter", "base"}
	dk := runtimeNode("box", "docker", 8000, 3000, 5000, now)

	if got := Pick([]*Node{fc, dk}, Request{MemMB: 1024, Template: "node"}, now, ttl); got == nil || got.ID != "box" {
		t.Fatalf("Pick(template=node) = %v, want box (wildcard node)", got)
	}
	if got := Pick([]*Node{fc}, Request{MemMB: 1024, Template: "base"}, now, ttl); got == nil || got.ID != "kvm" {
		t.Fatalf("Pick(template=base) = %v, want kvm", got)
	}
	if got := Pick([]*Node{fc}, Request{MemMB: 1024, Template: "playwright"}, now, ttl); got != nil {
		t.Fatalf("Pick(template=playwright) with only a limited fc node = %v, want nil", got)
	}
	if got := Pick([]*Node{fc}, Request{MemMB: 1024}, now, ttl); got == nil {
		t.Fatalf("Pick(no template) must still place on the fc node")
	}
}

func TestNilAndEmptyTemplateListsAreDifferentAnswers(t *testing.T) {
	docker := &Node{Templates: nil, CanFetchTemplates: true}
	if !docker.holdsTemplate("base") {
		t.Error("a node that does not enumerate was treated as holding nothing — it pulls on " +
			"demand, so no fetch stands between it and booting, and demoting it to the fetch " +
			"tier idles it behind Firecracker nodes for a fetch it never performs")
	}
	cold := &Node{Templates: []string{}, CanFetchTemplates: true}
	if cold.holdsTemplate("base") {
		t.Error("a node that enumerates and holds nothing was treated as unrestricted — it wins " +
			"on fit in the preferred tier and then pays the fetch that preference exists to avoid")
	}
	if !cold.hostsTemplate("base") {
		t.Error("a cold node with a shared store is not eligible at all — it can fetch")
	}
}

// omitempty on nodeRecord.Templates would turn [] into nil across the Redis round trip.
func TestTheStoredRecordKeepsAnEmptyTemplateList(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []string
		want bool
	}{
		{"enumerates, holds nothing", []string{}, false},
		{"does not enumerate", nil, true},
		{"holds one", []string{"base"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blob, err := json.Marshal(nodeRecord{ID: "n1", Templates: tc.in, CanFetch: true})
			if err != nil {
				t.Fatal(err)
			}
			var back nodeRecord
			if err := json.Unmarshal(blob, &back); err != nil {
				t.Fatal(err)
			}
			n := &Node{Templates: back.Templates, CanFetchTemplates: back.CanFetch}
			if got := n.holdsTemplate("base"); got != tc.want {
				t.Errorf("after a store round trip holdsTemplate = %v, want %v (stored as %s)",
					got, tc.want, blob)
			}
		})
	}
}

// A create naming network tags must land only where the tags can be enforced.
// The control is the same fleet with the flag off the request: it then lands on
// the node without the capability, which is the best fit, so the filter is the
// only thing moving the placement.
func TestPickFiltersByTagNetworks(t *testing.T) {
	now := time.Now()
	ttl := 30 * time.Second
	plain := runtimeNode("plain", "firecracker", 8000, 3000, 5000, now) // tighter fit, wins by default
	tagged := runtimeNode("tagged", "firecracker", 64000, 40000, 20000, now)
	tagged.TagNetworks = true

	if got := Pick([]*Node{plain, tagged}, Request{MemMB: 1024}, now, ttl); got == nil || got.ID != "plain" {
		t.Fatalf("Pick(no tags) = %v, want plain (best fit, control)", got)
	}
	got := Pick([]*Node{plain, tagged}, Request{MemMB: 1024, TagNetworks: true}, now, ttl)
	if got == nil || got.ID != "tagged" {
		t.Fatalf("Pick(TagNetworks) = %v, want tagged", got)
	}
	if got := Pick([]*Node{plain}, Request{MemMB: 1024, TagNetworks: true}, now, ttl); got != nil {
		t.Fatalf("Pick(TagNetworks) with no capable node = %v, want nil", got)
	}
}

// Best fit still packs, but not onto a node whose CPU is already saturated
// while a cooler one has room; with no cool node the hot one still serves.
func TestPickPassesOverACPUHotNode(t *testing.T) {
	now := time.Now()
	ttl := 30 * time.Second
	tight := node("tight", 16000, 4000, 8000, 7.5, now) // 8 cores at load 7.5: hot
	tight.Cap.Cores = 8
	roomy := node("roomy", 16000, 12000, 2000, 1, now)
	roomy.Cap.Cores = 8
	if got := Pick([]*Node{tight, roomy}, Request{MemMB: 1024}, now, ttl); got == nil || got.ID != "roomy" {
		t.Fatalf("Pick = %v, want roomy: tight is the better fit but CPU-hot", got)
	}
	tight.Cap.Load1 = 2
	if got := Pick([]*Node{tight, roomy}, Request{MemMB: 1024}, now, ttl); got == nil || got.ID != "tight" {
		t.Fatalf("Pick = %v, want tight: cooled down, best fit wins again", got)
	}
	tight.Cap.Load1 = 7.5
	if got := Pick([]*Node{tight}, Request{MemMB: 1024}, now, ttl); got == nil || got.ID != "tight" {
		t.Fatalf("Pick = %v, want tight: a hot node still serves when it is the only fit", got)
	}
}
