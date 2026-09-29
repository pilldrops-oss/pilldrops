package store

import (
	"context"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/egress"
)

func TestOrgEgressAbsentIsNotPermissive(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rules, ok, err := s.OrgEgress(ctx, "nobody")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("an org with no override reported one")
	}
	if rules != nil {
		t.Errorf("an absent override returned rules: %+v", rules)
	}
}

func TestOrgEgressDenyAllRoundTrips(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.SetOrgEgress(ctx, "locked", egress.DenyAll()); err != nil {
		t.Fatal(err)
	}
	rules, ok, err := s.OrgEgress(ctx, "locked")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || rules == nil {
		t.Fatal("a stored deny-all policy came back as absent, which the caller reads as unrestricted")
	}
	allow, deny, err := rules.Parse()
	if err != nil {
		t.Fatal(err)
	}
	if v := egress.Match(allow, deny, egress.Conn{Name: "anywhere.example", Port: 443}); v.Allowed {
		t.Errorf("a stored deny-all policy permitted traffic after a round trip: %s", v.Reason)
	}
}

func TestOrgEgressRoundTripsEntries(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	want := &egress.Rules{Allow: []string{"*.github.com", "8.8.8.0/24"}, Deny: []string{egress.AllTraffic}}
	if err := s.SetOrgEgress(ctx, "acme", want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.OrgEgress(ctx, "acme")
	if err != nil || !ok {
		t.Fatalf("read back: ok=%v err=%v", ok, err)
	}
	if len(got.Allow) != 2 || got.Allow[0] != "*.github.com" || got.Deny[0] != egress.AllTraffic {
		t.Errorf("entries did not survive the round trip: %+v", got)
	}
}

func TestSetOrgEgressReplaces(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.SetOrgEgress(ctx, "acme", &egress.Rules{Allow: []string{"*.github.com", "*.npmjs.org"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOrgEgress(ctx, "acme", &egress.Rules{Allow: []string{"*.github.com"}}); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.OrgEgress(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Allow) != 1 {
		t.Errorf("the second write merged with the first instead of replacing it: %+v", got.Allow)
	}
}

func TestSetOrgEgressNilClears(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.SetOrgEgress(ctx, "acme", egress.DenyAll()); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOrgEgress(ctx, "acme", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.OrgEgress(ctx, "acme"); err != nil || ok {
		t.Errorf("nil did not clear the override: ok=%v err=%v", ok, err)
	}
}

func TestSetOrgEgressRefusesInvalidRules(t *testing.T) {
	s := openTestStore(t)
	if err := s.SetOrgEgress(context.Background(), "acme", &egress.Rules{Allow: []string{"::1"}}); err == nil {
		t.Error("an IPv6 entry was stored; nothing on the node evaluates IPv6, so it would " +
			"sit in the policy reading as protection")
	}
}
