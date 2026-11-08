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

// The newer platform labels are refused only for a new assignment: they stay
// valid slugs, so a sandbox that already holds one keeps routing.
func TestNewlyReservedSlugsAreRefusedOnlyForNewAssignments(t *testing.T) {
	for _, s := range []string{"login", "auth", "accounts", "clerk", "admin", "docs", "help", "support", "status", "billing", "share", "agent"} {
		if err := ValidSlug(s); err != nil {
			t.Errorf("ValidSlug(%q) = %v: an existing holder would stop routing", s, err)
		}
		if err := AssignableSlug(s); err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Errorf("AssignableSlug(%q) = %v, want reserved", s, err)
		}
		if !ReservedForNewSlugs(s) {
			t.Errorf("ReservedForNewSlugs(%q) = false", s)
		}
	}
	if err := AssignableSlug("myapp"); err != nil {
		t.Errorf("AssignableSlug(myapp) = %v", err)
	}
	if err := AssignableSlug("www"); err == nil {
		t.Error("AssignableSlug(www) = nil: the older reserved labels must still be refused")
	}
}
