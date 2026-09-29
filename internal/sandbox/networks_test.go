package sandbox

import "testing"

func TestValidNetworks(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		tags []string
		ok   bool
	}{
		{"none", nil, true},
		{"empty list is untagged", []string{}, true},
		{"one", []string{"web"}, true},
		{"digits and hyphens", []string{"team-1", "0"}, true},
		{"eight", []string{"a", "b", "c", "d", "e", "f", "g", "h"}, true},
		{"nine", []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}, false},
		{"uppercase", []string{"Web"}, false},
		{"space", []string{"a b"}, false},
		{"leading hyphen", []string{"-a"}, false},
		{"blank", []string{""}, false},
		{"33 chars", []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, false},
		{"duplicate", []string{"web", "web"}, false},
		{"dot", []string{"a.b"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidNetworks(c.tags)
			if c.ok && err != nil {
				t.Fatalf("ValidNetworks(%q) = %v, want nil", c.tags, err)
			}
			if !c.ok && err == nil {
				t.Fatalf("ValidNetworks(%q) = nil, want a refusal naming the rule", c.tags)
			}
		})
	}
}

// Absent and [] must be the same thing, since the difference would otherwise
// decide whether a sandbox is on a network.
func TestSpecValidateNetworks(t *testing.T) {
	t.Parallel()
	known := func(string) bool { return true }
	for _, tags := range [][]string{nil, {}} {
		if _, err := (Spec{Template: "base", Networks: tags}).Validate(known, nil, nil); err != nil {
			t.Fatalf("Validate(networks=%q) = %v, want nil", tags, err)
		}
	}
	_, err := (Spec{Template: "base", Networks: []string{"a b"}}).Validate(known, nil, nil)
	if err == nil {
		t.Fatal("Validate accepted a network tag with a space")
	}
	if _, ok := err.(*InvalidError); !ok {
		t.Fatalf("Validate returned %T, want *InvalidError so the HTTP layer answers 400", err)
	}
}
