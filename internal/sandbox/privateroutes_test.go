package sandbox

import (
	"net/netip"
	"slices"
	"strings"
	"testing"
)

func prefixes(t *testing.T, ss ...string) []netip.Prefix {
	t.Helper()
	out := make([]netip.Prefix, 0, len(ss))
	for _, s := range ss {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}

func strs(ps []netip.Prefix) []string {
	out := []string{}
	for _, p := range ps {
		out = append(out, p.String())
	}
	return out
}

func TestParsePrivateSubnet(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"10.0.0.0/8", "10.0.5.0/24", "192.0.2.7/32", " 10.1.0.0/16 "} {
		if _, err := ParsePrivateSubnet(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"0.0.0.0/0", "10.0.0.0/7", "10.0.5.1/24", "fd00::/8", "10.0.0.0", "nope", ""} {
		if _, err := ParsePrivateSubnet(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestCarvePlatform(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		in, want []string
	}{
		{[]string{"10.0.5.0/24"}, []string{"10.0.5.0/24"}},
		// Wholly reserved: nothing is left to route, which the grant API refuses.
		{[]string{"10.128.0.0/16"}, []string{}},
		{[]string{"169.254.169.0/24"}, []string{}},
		{[]string{"10.0.0.0/8"}, []string{"10.0.0.0/9", "10.129.0.0/16", "10.130.0.0/15", "10.132.0.0/14",
			"10.136.0.0/13", "10.144.0.0/12", "10.160.0.0/11", "10.192.0.0/10"}},
		{[]string{"192.168.0.0/16"}, []string{"192.168.0.0/20", "192.168.16.0/21", "192.168.24.0/22",
			"192.168.28.0/23", "192.168.32.0/19", "192.168.64.0/18", "192.168.128.0/17"}},
		// Nested grants fold into the parent; nft refuses overlapping interval elements.
		{[]string{"10.0.5.0/24", "10.0.0.0/16", "10.0.5.0/24"}, []string{"10.0.0.0/16"}},
	} {
		if got := strs(CarvePlatform(prefixes(t, c.in...))); !slices.Equal(got, c.want) {
			t.Errorf("CarvePlatform(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestShadowed(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		in   string
		want []string
	}{
		{"10.0.5.0/24", []string{}},
		{"10.0.0.0/8", []string{"10.128.0.0/16"}},
		{"169.254.169.0/24", []string{"169.254.169.0/24"}},
		{"100.0.0.0/8", []string{"100.64.0.0/10"}},
	} {
		if got := Shadowed(netip.MustParsePrefix(c.in)); !slices.Equal(got, c.want) {
			t.Errorf("Shadowed(%s) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestValidPrivateSuffix(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"corp.internal", "a.b.c", "x-1.corp", strings.Repeat("a", 63) + ".corp"} {
		if err := ValidPrivateSuffix(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"internal", "Corp.internal", "corp.internal.", "x.pd", "x.private", "x.localhost", "a_b.corp", "10.0.0.1", "1.2.3.4", "", strings.Repeat("a", 64) + ".corp", strings.Repeat("a.", 127) + "co"} {
		if ValidPrivateSuffix(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestLongestSuffix(t *testing.T) {
	t.Parallel()
	sfx := []string{"corp.internal", "wiki.corp.internal"}
	if got, ok := LongestSuffix("a.wiki.corp.internal", sfx); !ok || got != "wiki.corp.internal" {
		t.Errorf("got %q %v", got, ok)
	}
	if got, ok := LongestSuffix("corp.internal", sfx); !ok || got != "corp.internal" {
		t.Errorf("the suffix itself: %q %v", got, ok)
	}
	if _, ok := LongestSuffix("notcorp.internal", sfx); ok {
		t.Error("a name sharing only a string tail matched")
	}
}

func TestValidPrivateRoutes(t *testing.T) {
	t.Parallel()
	if err := ValidPrivateRoutes(PrivateRoutes{Subnets: []string{"10.0.0.0/9"}, DNS: []string{"corp.internal"}}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []PrivateRoutes{
		{Subnets: []string{"10.128.0.0/16"}},
		{Subnets: []string{"10.0.5.1/24"}},
		{Subnets: []string{"fd00::/8"}},
		{DNS: []string{"internal"}},
	} {
		if ValidPrivateRoutes(bad) == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	if !(PrivateRoutes{}).Empty() || (PrivateRoutes{DNS: []string{"a.b"}}).Empty() {
		t.Error("Empty is wrong")
	}
}
