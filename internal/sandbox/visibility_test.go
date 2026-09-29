package sandbox

import "testing"

// The whole rule, including the two shapes a legacy row takes: visibility ""
// (the column did not exist) and created_by "" (nobody was recorded). Both
// must read as "the org sees it", or the migration hides every existing
// sandbox from everyone but its creator, which nobody has.
func TestVisible(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		visibility, createdBy, caller string
		operator                      bool
		want                          bool
	}{
		{"legacy row, empty visibility", "", "", "key_bob", false, true},
		{"legacy row, empty visibility, no caller key", "", "", "", false, true},
		{"org-visible to another member", VisibilityOrg, "key_alice", "key_bob", false, true},
		{"private to its creator", VisibilityPrivate, "key_alice", "key_alice", false, true},
		{"private to another member", VisibilityPrivate, "key_alice", "key_bob", false, false},
		{"private to an operator", VisibilityPrivate, "key_alice", "key_bob", true, true},
		{"private with no creator, ordinary member", VisibilityPrivate, "", "key_bob", false, false},
		// The one that would be a hole: a caller with no key id (an agent
		// token, an unsigned hop) must not match a creatorless private row.
		{"private with no creator, caller with no key", VisibilityPrivate, "", "", false, false},
		{"private with no creator, operator", VisibilityPrivate, "", "", true, true},
		// A sandbox is never hidden from itself, whoever created it.
		{"private to its own identity", VisibilityPrivate, "key_alice", SelfKeyID("sb-1"), false, true},
		{"private with no creator, its own identity", VisibilityPrivate, "", SelfKeyID("sb-1"), false, true},
		{"private to ANOTHER sandbox's identity", VisibilityPrivate, "key_alice", SelfKeyID("sb-2"), false, false},
	} {
		if got := Visible("sb-1", tc.visibility, tc.createdBy, tc.caller, tc.operator); got != tc.want {
			t.Errorf("%s: Visible(sb-1, %q, %q, %q, operator=%v) = %v, want %v",
				tc.name, tc.visibility, tc.createdBy, tc.caller, tc.operator, got, tc.want)
		}
	}
	// The self rule must not turn into "any sandbox principal": an unknown id
	// (the anonymous form below) has no self to match.
	if Visible("", VisibilityPrivate, "key_alice", SelfKeyID(""), false) {
		t.Error("a private sandbox was visible to a self key for no sandbox at all")
	}
}

// The data path's caller: a raw public port has none. Written against the same
// table rows the API rule uses, so the two cannot be given different answers.
func TestReachableAnonymously(t *testing.T) {
	for _, tc := range []struct {
		visibility, createdBy string
		want                  bool
	}{
		{"", "", true},
		{VisibilityOrg, "key_alice", true},
		{VisibilityPrivate, "key_alice", false},
		{VisibilityPrivate, "", false},
	} {
		if got := ReachableAnonymously(tc.visibility, tc.createdBy); got != tc.want {
			t.Errorf("ReachableAnonymously(%q, %q) = %v, want %v", tc.visibility, tc.createdBy, got, tc.want)
		}
	}
}

func TestValidVisibility(t *testing.T) {
	for in, want := range map[string]string{"": VisibilityOrg, "org": "org", "private": "private"} {
		got, err := ValidVisibility(in)
		if err != nil || got != want {
			t.Errorf("ValidVisibility(%q) = (%q, %v), want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"public", "ORG", "shared", "none"} {
		if _, err := ValidVisibility(bad); err == nil {
			t.Errorf("ValidVisibility(%q) was accepted", bad)
		}
	}
}
