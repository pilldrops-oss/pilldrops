package sandbox

import "testing"

func TestValidPrivateName(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"db", "a", "api-2", "x123456789012345678901234567890123456789012345678901234567890ab"} {
		if !ValidPrivateName(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "DB", "-db", "db-", "d_b", "db.private", "x1234567890123456789012345678901234567890123456789012345678901abc"} {
		if ValidPrivateName(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestValidPrivateTargets(t *testing.T) {
	t.Parallel()
	if err := ValidPrivateTargets([]PrivateTarget{{Name: "db", Port: 5432}, {Name: "api", Host: "internal.corp", Port: 443}}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]PrivateTarget{
		{{Name: "db", Port: 0}},
		{{Name: "db", Port: 70000}},
		{{Name: "db", Port: 1}, {Name: "db", Port: 2}},
		{{Name: "Bad", Port: 1}},
	} {
		if ValidPrivateTargets(bad) == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	many := make([]PrivateTarget, MaxPrivateTargets+1)
	for i := range many {
		many[i] = PrivateTarget{Name: "t" + string(rune('a'+i%26)) + string(rune('a'+i/26)), Port: 1}
	}
	if ValidPrivateTargets(many) == nil {
		t.Error("more than the limit accepted")
	}
}
