package aat

import (
	"strings"
	"testing"
)

func TestMintProducesDistinctTokensThatVerify(t *testing.T) {
	a, b := Mint(), Mint()
	if a.Secret == b.Secret || a.ID == b.ID || a.SecretSHA256 == b.SecretSHA256 {
		t.Fatal("Mint repeated itself")
	}
	if !VerifyHash(a.Secret, a.SecretSHA256) {
		t.Fatal("a minted token does not verify against its own hash")
	}
	if VerifyHash(a.Secret, b.SecretSHA256) {
		t.Fatal("a token verified against the WRONG hash")
	}
	if !strings.HasPrefix(a.Secret, Prefix) {
		t.Fatalf("missing prefix: %q", a.Secret)
	}
}

func TestVerifyRejectsEmptyInputs(t *testing.T) {
	tok := Mint()
	// Without the guard, "" compares equal to "" and a blank hash column accepts a blank token.
	for _, c := range []struct{ token, hash string }{
		{"", tok.SecretSHA256}, {tok.Secret, ""}, {"", ""},
	} {
		if VerifyHash(c.token, c.hash) {
			t.Fatalf("VerifyHash(%q, %q) authenticated", c.token, c.hash)
		}
	}
}

func TestHashCoversTheIDNotJustTheSecret(t *testing.T) {
	tok := Mint()
	// Swap the public id, keep the secret half.
	_, rest, _ := strings.Cut(strings.TrimPrefix(tok.Secret, Prefix), "_")
	tampered := Prefix + "aaaaaaaaaaaa" + "_" + rest
	if VerifyHash(tampered, tok.SecretSHA256) {
		t.Fatal("a token with a rewritten id still verified")
	}
}

func TestParseIDAcceptsOnlyWellFormedTokens(t *testing.T) {
	tok := Mint()
	id, err := ParseID(tok.Secret)
	if err != nil {
		t.Fatalf("ParseID on a minted token: %v", err)
	}
	if id != tok.ID {
		t.Fatalf("ParseID = %q, want %q", id, tok.ID)
	}

	for _, bad := range []string{
		"",
		"sfa_",
		"sfa_abc",                            // no separator
		"sfa__" + strings.Repeat("a", 32),    // empty id
		"sfa_abc_",                           // empty secret
		"sfa_abc_short",                      // secret too short
		"sfa_ABC_" + strings.Repeat("a", 32), // upper-case id
		"sfa_zzz_" + strings.Repeat("a", 32), // non-hex id
		"sf_live_abcdefghijklmnop",           // an org key
		Prefix,
	} {
		if _, err := ParseID(bad); err == nil {
			t.Fatalf("ParseID(%q) accepted", bad)
		}
	}
}

func TestParseIDNeverPanics(t *testing.T) {
	for _, s := range []string{
		"sfa_", "sfa", "s", "\x00", "sfa_\x00_aaaaaaaaaaaaaaaaaaaa",
		"sfa_" + strings.Repeat("_", 100),
		"sfa_a_" + strings.Repeat("\xff", 40),
		strings.Repeat("sfa_", 1000),
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("ParseID(%q) panicked: %v", s, r)
				}
			}()
			_, _ = ParseID(s)
		}()
	}
}

func FuzzParseID(f *testing.F) {
	f.Add("sfa_abcdef_0123456789abcdef0123456789abcdef")
	f.Add("sfa__")
	f.Add("sf_live_x")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		id, err := ParseID(s)
		if err != nil {
			return
		}
		if id == "" || !isHex(id) || !strings.Contains(s, id) {
			t.Fatalf("ParseID(%q) returned a bad id %q", s, id)
		}
	})
}

func TestScopeSetRoundTripsThroughStorage(t *testing.T) {
	set, err := NewSet([]Scope{ScopeConsole, ScopeRead, ScopeAdmin})
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	enc := set.Encode()
	if enc != "agent:admin agent:console agent:read" {
		t.Fatalf("Encode = %q", enc)
	}
	back := DecodeSet(enc)
	if len(back) != 3 || !back.Has(ScopeConsole) || !back.Has(ScopeAdmin) || !back.Has(ScopeRead) {
		t.Fatalf("round trip lost scopes: %q", back.Encode())
	}
}

func TestNewSetRejectsUnknownAndEmpty(t *testing.T) {
	if _, err := NewSet([]Scope{ScopeRead, Scope("agent:teleport")}); err == nil {
		t.Fatal("an unknown scope was accepted at mint time")
	}
	if _, err := NewSet(nil); err == nil {
		t.Fatal("a token with no scopes was accepted")
	}
	if _, err := NewSet([]Scope{Scope("Agent:Read")}); err == nil {
		t.Fatal("a differently-cased scope was accepted")
	}
}

func TestDecodeSetDropsUnknownScopesInsteadOfFailing(t *testing.T) {
	set := DecodeSet("agent:read agent:teleport   agent:write")
	if len(set) != 2 || !set.Has(ScopeRead) || !set.Has(ScopeWrite) {
		t.Fatalf("DecodeSet mishandled an unknown scope: %q", set.Encode())
	}
	if len(DecodeSet("")) != 0 {
		t.Fatal("an empty column decoded to a non-empty set")
	}
}

func TestNoScopeImpliesAnother(t *testing.T) {
	admin, err := NewSet([]Scope{ScopeAdmin})
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	for _, s := range All {
		if s == ScopeAdmin {
			continue
		}
		if admin.Has(s) {
			t.Fatalf("agent:admin implied %s", s)
		}
	}
	write, _ := NewSet([]Scope{ScopeWrite})
	if write.Has(ScopeConsole) {
		t.Fatal("agent:write implied a shell")
	}
}

func TestHasAnyFindsOneOfSeveral(t *testing.T) {
	set, _ := NewSet([]Scope{ScopeFiles})
	if !set.HasAny(ScopeConsole, ScopeFiles) {
		t.Fatal("HasAny missed a granted scope")
	}
	if set.HasAny(ScopeConsole, ScopeAdmin) {
		t.Fatal("HasAny matched a scope that was not granted")
	}
}

func TestEveryScopeIsDescribedAsAConsequence(t *testing.T) {
	for _, s := range All {
		d := Describe(s)
		if d == "" || d == string(s) {
			t.Fatalf("scope %s has no real description", s)
		}
		if !strings.HasSuffix(d, ".") {
			t.Fatalf("scope %s description is not a sentence: %q", s, d)
		}
	}
	if !strings.Contains(Describe(ScopeConsole), "any command") {
		t.Fatalf("console's description understates it: %q", Describe(ScopeConsole))
	}
}

func TestAllIsOrderedLeastToMostDangerous(t *testing.T) {
	if All[0] != ScopeRead {
		t.Fatalf("All starts with %s, want the least dangerous", All[0])
	}
	if All[len(All)-1] != ScopeConsole {
		t.Fatalf("All ends with %s, want console last", All[len(All)-1])
	}
	if len(All) != 6 {
		t.Fatalf("All has %d scopes; a new one needs a place in this order", len(All))
	}
}

func TestRandomCodeIsUnguessableAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		c := RandomCode()
		if len(c) != 32 {
			t.Fatalf("RandomCode length %d", len(c))
		}
		if seen[c] {
			t.Fatalf("RandomCode repeated: %s", c)
		}
		seen[c] = true
	}
}
