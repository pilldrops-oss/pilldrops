package orgassert

import (
	"crypto/ed25519"
	"strings"
	"testing"
	"time"
)

func testKeys(t *testing.T) (ed25519.PrivateKey, ed25519.PublicKey) {
	t.Helper()
	priv, err := ParsePrivateKey("00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	if err != nil {
		t.Fatalf("ParsePrivateKey: %v", err)
	}
	pub, err := ParsePublicKey(PublicKeyHex(priv))
	if err != nil {
		t.Fatalf("ParsePublicKey: %v", err)
	}
	return priv, pub
}

func TestAsymRoundTripAndOperationBinding(t *testing.T) {
	priv, pub := testKeys(t)
	now := time.Now()
	tok := SignAsym(priv, "u:cust9", "node-a", "GET", "/v1/sandboxes", now)
	if tok == "" {
		t.Fatal("SignAsym returned empty")
	}
	if org, _, ok := VerifyAsym(pub, tok, "node-a", "GET", "/v1/sandboxes", now); !ok || org != "u:cust9" {
		t.Fatalf("VerifyAsym = (%q, %v), want (u:cust9, true)", org, ok)
	}
	// Same binding guarantees as the HMAC scheme.
	if _, _, ok := VerifyAsym(pub, tok, "node-a", "DELETE", "/v1/sandboxes/sb-victim", now); ok {
		t.Error("an assertion for GET verified for a DELETE on another path")
	}
	if _, _, ok := VerifyAsym(pub, tok, "node-a", "GET", "/v1/sandboxes", now.Add(TTL+MaxSkew+time.Second)); ok {
		t.Error("an expired assertion verified")
	}
}

func TestAPublicKeyHolderCannotMintAnAssertion(t *testing.T) {
	priv, pub := testKeys(t)
	now := time.Now()

	// Feeding the public key to either signing entry point must produce nothing that verifies.
	if tok := SignAsym(ed25519.PrivateKey(pub), "u:victim", "node-a", "GET", "/v1/sandboxes", now); tok != "" {
		if _, _, ok := VerifyAsym(pub, tok, "node-a", "GET", "/v1/sandboxes", now); ok {
			t.Fatal("a public key was accepted as a signing key")
		}
	}
	hmacTok := Sign(pub, "u:victim", "node-a", "GET", "/v1/sandboxes", now)
	if hmacTok != "" {
		if _, _, ok := VerifyAsym(pub, hmacTok, "node-a", "GET", "/v1/sandboxes", now); ok {
			t.Fatal("an HMAC token keyed on the public key verified as an Ed25519 assertion")
		}
	}
	// Sanity: the genuine key still verifies, so the above is not passing because everything fails.
	if _, _, ok := VerifyAsym(pub, SignAsym(priv, "u:cust9", "node-a", "GET", "/v1/sandboxes", now), "node-a", "GET", "/v1/sandboxes", now); !ok {
		t.Fatal("the genuine key does not verify — the test proves nothing")
	}
}

func TestTheTwoSchemesDoNotCrossValidate(t *testing.T) {
	priv, pub := testKeys(t)
	now := time.Now()
	secret := []byte("shared-secret")

	asym := SignAsym(priv, "u:cust9", "node-a", "GET", "/v1/sandboxes", now)
	if _, _, ok := Verify(secret, asym, "node-a", "GET", "/v1/sandboxes", now); ok {
		t.Error("an Ed25519 assertion verified as HMAC")
	}
	sym := Sign(secret, "u:cust9", "node-a", "GET", "/v1/sandboxes", now)
	if _, _, ok := VerifyAsym(pub, sym, "node-a", "GET", "/v1/sandboxes", now); ok {
		t.Error("an HMAC assertion verified as Ed25519")
	}
}

func TestKeyParsingRefusesBadInput(t *testing.T) {
	for _, bad := range []string{"nothex", "aabb", strings.Repeat("a", 63), strings.Repeat("a", 66)} {
		if _, err := ParsePrivateKey(bad); err == nil {
			t.Errorf("ParsePrivateKey(%q) accepted a bad key", bad)
		}
		if _, err := ParsePublicKey(bad); err == nil {
			t.Errorf("ParsePublicKey(%q) accepted a bad key", bad)
		}
	}
	// Empty means "not configured", not an error.
	if k, err := ParsePrivateKey(""); err != nil || k != nil {
		t.Errorf("ParsePrivateKey(\"\") = (%v, %v), want (nil, nil)", k, err)
	}
}

func TestBothSchemesRejectTheSameMalformedTokens(t *testing.T) {
	_, pub := testKeys(t)
	secret := []byte("shared-secret")
	now := time.Now()

	for _, tok := range []string{
		"", "|", "no-pipes", "org|orchestrator", "org|orchestrator|notanumber|sig",
		"|orchestrator|123|sig", "org|WRONGCALLER|123|sig", "org|orchestrator|123|",
	} {
		if _, _, ok := VerifyAsym(pub, tok, "node-a", "GET", "/v1/sandboxes", now); ok {
			t.Errorf("VerifyAsym accepted malformed %q", tok)
		}
		if _, _, ok := Verify(secret, tok, "node-a", "GET", "/v1/sandboxes", now); ok {
			t.Errorf("Verify accepted malformed %q", tok)
		}
	}
}

// Bad base64 and wrong-length signatures must fail rather than panic.
func TestAsymRejectsUnusableSignatures(t *testing.T) {
	_, pub := testKeys(t)
	now := time.Now()
	for _, sig := range []string{"!!!not-base64!!!", "AAAA", ""} {
		tok := "u:cust9|orchestrator|" + "99999999999" + "|" + sig
		if _, _, ok := VerifyAsym(pub, tok, "node-a", "GET", "/v1/sandboxes", now); ok {
			t.Errorf("accepted signature %q", sig)
		}
	}
}

func TestPublicKeyHexRefusesAnUnusableKey(t *testing.T) {
	if got := PublicKeyHex(nil); got != "" {
		t.Errorf("PublicKeyHex(nil) = %q, want empty", got)
	}
	if got := PublicKeyHex(make([]byte, 5)); got != "" {
		t.Errorf("PublicKeyHex(short) = %q, want empty", got)
	}
	priv, _ := testKeys(t)
	if got := PublicKeyHex(priv); len(got) != 64 {
		t.Errorf("PublicKeyHex len = %d, want 64 hex chars", len(got))
	}
}

func TestParsePublicKeyAcceptsEmptyAsUnconfigured(t *testing.T) {
	k, err := ParsePublicKey("")
	if err != nil || k != nil {
		t.Errorf("ParsePublicKey(\"\") = (%v, %v), want (nil, nil)", k, err)
	}
}
