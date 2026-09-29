package previewtoken

import "testing"

func TestTokenValidRoundTrip(t *testing.T) {
	secret := []byte("shared-secret")
	tok := Token(secret, "sb-1", 8000, "u:cust1")
	if tok == "" {
		t.Fatal("Token returned empty for a valid secret/id")
	}
	if !Valid(secret, tok, "sb-1", 8000, "u:cust1") {
		t.Fatal("Valid(fresh token) = false, want true")
	}
}

func TestValidRejectsWrongOrg(t *testing.T) {
	secret := []byte("shared-secret")
	tok := Token(secret, "sb-1", 8000, "u:cust1")
	if Valid(secret, tok, "sb-1", 8000, "u:cust2") {
		t.Fatal("token minted for u:cust1 validated for u:cust2")
	}
}

func TestValidRejectsWrongPort(t *testing.T) {
	secret := []byte("shared-secret")
	tok := Token(secret, "sb-1", 8000, "u:cust1")
	if Valid(secret, tok, "sb-1", 9000, "u:cust1") {
		t.Fatal("token minted for port 8000 validated for port 9000")
	}
}

func TestValidRejectsWrongID(t *testing.T) {
	secret := []byte("shared-secret")
	tok := Token(secret, "sb-1", 8000, "u:cust1")
	if Valid(secret, tok, "sb-2", 8000, "u:cust1") {
		t.Fatal("token minted for sb-1 validated for sb-2 (cross-sandbox replay)")
	}
}

func TestValidRejectsWrongSecret(t *testing.T) {
	tok := Token([]byte("real-secret"), "sb-1", 8000, "u:cust1")
	if Valid([]byte("guessed-secret"), tok, "sb-1", 8000, "u:cust1") {
		t.Fatal("forged/wrong-secret token validated")
	}
}

func TestTokenEmptySecretRefuses(t *testing.T) {
	if tok := Token(nil, "sb-1", 8000, "u:cust1"); tok != "" {
		t.Errorf("Token with no secret = %q, want empty", tok)
	}
}

func TestValidEmptySecretAlwaysRefuses(t *testing.T) {
	tok := Token([]byte("some-secret"), "sb-1", 8000, "u:cust1")
	if Valid(nil, tok, "sb-1", 8000, "u:cust1") {
		t.Fatal("Valid with empty secret validated a real token")
	}
}

func TestValidEmptyTokenRefuses(t *testing.T) {
	if Valid([]byte("secret"), "", "sb-1", 8000, "u:cust1") {
		t.Fatal("Valid with empty token string validated")
	}
}

// The org-blind form is a distinct credential, not a wildcard.
func TestOrgBlindTokenDoesNotMatchOrgBound(t *testing.T) {
	secret := []byte("shared-secret")
	blind := Token(secret, "sb-1", 8000, "")
	if Valid(secret, blind, "sb-1", 8000, "u:cust1") {
		t.Fatal("org-blind token validated against a specific org")
	}
	if !Valid(secret, blind, "sb-1", 8000, "") {
		t.Fatal("org-blind token failed to validate against its own (empty) org")
	}
}

func TestSignDeterministicPerSecretNotGuessable(t *testing.T) {
	a := Token([]byte("secret-a"), "sb-1", 8000, "u:cust1")
	b := Token([]byte("secret-b"), "sb-1", 8000, "u:cust1")
	if a == b {
		t.Fatal("Token produced identical output under different secrets")
	}
}

func TestAuthorizedFailsClosedWhenSecretUnsetAndNotDevInsecure(t *testing.T) {
	tok := Token([]byte("some-secret"), "sb-1", 8000, "u:cust1")
	if Authorized(nil, tok, "sb-1", 8000, "u:cust1", false) {
		t.Fatal("Authorized with no secret configured and devInsecure=false must refuse")
	}
	if Authorized(nil, "", "sb-1", 8000, "u:cust1", false) {
		t.Fatal("Authorized with no secret and no token must refuse")
	}
}

func TestAuthorizedDevInsecureFallsBackWhenSecretUnset(t *testing.T) {
	if !Authorized(nil, "", "sb-1", 8000, "u:cust1", true) {
		t.Fatal("Authorized with no secret and devInsecure=true must allow (the dev escape hatch)")
	}
}

func TestAuthorizedOrgBoundToken(t *testing.T) {
	secret := []byte("shared-secret")
	tok := Token(secret, "sb-1", 8000, "u:cust1")
	if !Authorized(secret, tok, "sb-1", 8000, "u:cust1", false) {
		t.Fatal("Authorized refused a correctly org-bound token")
	}
}

func TestAuthorizedCrossOrgTokenRefused(t *testing.T) {
	secret := []byte("shared-secret")
	tok := Token(secret, "sb-1", 8000, "u:other-org")
	if Authorized(secret, tok, "sb-1", 8000, "u:cust1", false) {
		t.Fatal("Authorized allowed a token minted for a different org")
	}
}

func TestAuthorizedOrgBlindTokenStillGated(t *testing.T) {
	secret := []byte("shared-secret")
	blind := Token(secret, "sb-1", 8000, "")
	if !Authorized(secret, blind, "sb-1", 8000, "u:cust1", false) {
		t.Fatal("Authorized refused the org-blind form (still secret-bound)")
	}
	if Authorized(secret, "wrong-token", "sb-1", 8000, "u:cust1", false) {
		t.Fatal("Authorized allowed a bogus token")
	}
}
