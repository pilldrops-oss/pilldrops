package sandbox

import (
	"strings"
	"testing"
)

func TestValidSlug(t *testing.T) {
	for _, s := range []string{"", "myapp", "a", "w1-api", "a-b-c", "x" + strings.Repeat("y", 62)} {
		if err := ValidSlug(s); err != nil {
			t.Errorf("ValidSlug(%q) = %v, want nil", s, err)
		}
	}
	for _, c := range []struct{ slug, rule string }{
		{"1abc", "start with a lowercase letter"},
		{"8000-x", "start with a lowercase letter"},
		{"-a", "start with a lowercase letter"},
		{"a-", "ending in a hyphen"},
		{"agent-x", "agent- prefix"},
		{"www", "reserved"},
		{"x" + strings.Repeat("y", 63), "longer than 63"},
		{"MyApp", "start with a lowercase letter"},
		{"myApp", "lowercase letters, digits and hyphens"},
		{"my_app", "lowercase letters, digits and hyphens"},
		{"café", "lowercase letters, digits and hyphens"},
		{"a.b", "lowercase letters, digits and hyphens"},
	} {
		err := ValidSlug(c.slug)
		if err == nil {
			t.Errorf("ValidSlug(%q) = nil, want an error naming %q", c.slug, c.rule)
			continue
		}
		if !strings.Contains(err.Error(), c.rule) {
			t.Errorf("ValidSlug(%q) = %q, want it to name %q", c.slug, err, c.rule)
		}
	}
}
