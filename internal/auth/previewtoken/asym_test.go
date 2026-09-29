package previewtoken

import (
	"crypto/ed25519"
	"strings"
	"testing"
	"time"
)

func hopKeys(t *testing.T) (ed25519.PrivateKey, ed25519.PublicKey) {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return priv, priv.Public().(ed25519.PublicKey)
}

func TestNodeHopRoundTripAndScope(t *testing.T) {
	priv, pub := hopKeys(t)
	now := time.Unix(1_800_000_000, 0)
	tok := TokenAsym(priv, "sb1", 3000, "u:acme", now.Add(5*time.Minute))
	if tok == "" {
		t.Fatal("TokenAsym returned empty")
	}
	if !AuthorizedAsym(pub, tok, "sb1", 3000, "u:acme", now) {
		t.Fatal("a genuine token was refused")
	}
	for _, tc := range []struct {
		name, id string
		port     int
		org      string
	}{
		{"another sandbox", "sb2", 3000, "u:acme"},
		{"another port", "sb1", 8080, "u:acme"},
		{"another tenant", "sb1", 3000, "u:other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if AuthorizedAsym(pub, tok, tc.id, tc.port, tc.org, now) {
				t.Errorf("a token for sb1:3000/u:acme also opened %s", tc.name)
			}
		})
	}
}

func TestNodeHopPublicKeyHolderCannotMint(t *testing.T) {
	priv, pub := hopKeys(t)
	now := time.Unix(1_800_000_000, 0)

	forged := TokenAsym(ed25519.PrivateKey(pub), "sb1", 3000, "u:acme", now.Add(time.Minute))
	if forged != "" && AuthorizedAsym(pub, forged, "sb1", 3000, "u:acme", now) {
		t.Fatal("a public key was usable as a signing key")
	}
	if AuthorizedAsym(pub, Token(pub, "sb1", 3000, "u:acme"), "sb1", 3000, "u:acme", now) {
		t.Fatal("an HMAC node-hop token verified as Ed25519")
	}
	// Sanity, so the above cannot pass because everything fails.
	if !AuthorizedAsym(pub, TokenAsym(priv, "sb1", 3000, "u:acme", now.Add(time.Minute)), "sb1", 3000, "u:acme", now) {
		t.Fatal("the genuine key does not verify — this test proves nothing")
	}
}

func TestNodeHopExpires(t *testing.T) {
	priv, pub := hopKeys(t)
	now := time.Unix(1_800_000_000, 0)
	tok := TokenAsym(priv, "sb1", 3000, "u:acme", now.Add(time.Minute))

	if !AuthorizedAsym(pub, tok, "sb1", 3000, "u:acme", now.Add(59*time.Second)) {
		t.Error("refused inside its window")
	}
	if AuthorizedAsym(pub, tok, "sb1", 3000, "u:acme", now.Add(2*time.Minute)) {
		t.Error("an expired node-hop token still validates")
	}
}

func TestNodeHopAcceptsTheOrgBlindForm(t *testing.T) {
	priv, pub := hopKeys(t)
	now := time.Unix(1_800_000_000, 0)
	blind := TokenAsym(priv, "sb1", 3000, "", now.Add(time.Minute))
	if !AuthorizedAsym(pub, blind, "sb1", 3000, "u:acme", now) {
		t.Error("an org-blind token was refused against a known-org sandbox")
	}
}

// One Ed25519 key signs both org assertions and preview hops.
func TestNodeHopIsDomainSeparated(t *testing.T) {
	_, pub := hopKeys(t)
	a := string(previewMsg("sb1", 3000, "u:acme", "abc"))
	if !strings.HasPrefix(a, "sandforge:preview-hop:") {
		t.Fatalf("preview message lost its domain prefix: %q", a)
	}
	if len(pub) == 0 {
		t.Fatal("no key")
	}
}

func TestNodeHopRefusesMalformed(t *testing.T) {
	priv, pub := hopKeys(t)
	now := time.Unix(1_800_000_000, 0)
	for _, tok := range []string{"", ".", "abc.", ".abc", "zz.notbase64!!", "no-dot-at-all"} {
		if AuthorizedAsym(pub, tok, "sb1", 3000, "u:acme", now) {
			t.Errorf("malformed token %q validated", tok)
		}
	}
	if TokenAsym(nil, "sb1", 3000, "u:acme", now) != "" {
		t.Error("TokenAsym minted with a nil key")
	}
	if TokenAsym(priv, "", 3000, "u:acme", now) != "" {
		t.Error("TokenAsym minted for an empty sandbox id")
	}
	if AuthorizedAsym(nil, "x.y", "sb1", 3000, "u:acme", now) {
		t.Error("an unconfigured public key validated a token")
	}
}
