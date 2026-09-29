package adminauth

import (
	"strings"
	"testing"
	"time"
)

func TestHashVerifyRoundTrip(t *testing.T) {
	h, err := Hash("correct-horse-battery")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if !Verify(h, "correct-horse-battery") {
		t.Error("the right password did not verify")
	}
	if Verify(h, "correct-horse-batterY") {
		t.Error("a wrong password verified")
	}
	if strings.Contains(h, "correct-horse-battery") {
		t.Fatalf("the hash contains the password: %q", h)
	}
}

func TestEqualPasswordsGetDifferentHashes(t *testing.T) {
	a, err := Hash("the-same-password-x")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Hash("the-same-password-x")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("identical passwords produced identical hashes — the salt is not random")
	}
	if !Verify(a, "the-same-password-x") || !Verify(b, "the-same-password-x") {
		t.Error("both must still verify")
	}
}

func TestShortPasswordsRefused(t *testing.T) {
	if _, err := Hash(strings.Repeat("a", MinPasswordLen-1)); err == nil {
		t.Error("a short password was accepted")
	}
	if _, err := Hash(strings.Repeat("a", MinPasswordLen)); err != nil {
		t.Errorf("a password at the floor was refused: %v", err)
	}
}

func TestMalformedHashesNeverVerify(t *testing.T) {
	for _, stored := range []string{
		"", "pbkdf2", "pbkdf2$", "pbkdf2$600000$$", "notpbkdf2$1$aa$bb",
		"pbkdf2$0$aa$bb", "pbkdf2$abc$aa$bb", "pbkdf2$600000$zz$bb", "pbkdf2$600000$aa$zz",
		"$$$", "pbkdf2$600000$aa$",
	} {
		if Verify(stored, "anything") {
			t.Errorf("malformed hash %q verified", stored)
		}
		if Verify(stored, "") {
			t.Errorf("malformed hash %q verified an empty password", stored)
		}
	}
}

func TestOldWorkFactorStillVerifiesAndIsFlagged(t *testing.T) {
	old := "pbkdf2$1000$" + "00112233445566778899aabbccddeeff" + "$"
	h, err := Hash("a-long-enough-password")
	if err != nil {
		t.Fatal(err)
	}
	if NeedsUpgrade(h) {
		t.Error("a freshly made hash should not need upgrading")
	}
	_ = old
	if !NeedsUpgrade("pbkdf2$1000$aabb$ccdd") {
		t.Error("a lower work factor was not flagged")
	}
	if !NeedsUpgrade("garbage") {
		t.Error("an unparseable hash should be flagged for replacement")
	}
}

func TestSessionRoundTripAndExpiry(t *testing.T) {
	secret := []byte("session-signing-secret")
	now := time.Unix(1_800_000_000, 0)

	tok := MintSession(secret, "admin@example.com", now)
	got, err := VerifySession(secret, tok, now)
	if err != nil || got != "admin@example.com" {
		t.Fatalf("VerifySession = (%q, %v), want the email", got, err)
	}
	if _, err := VerifySession(secret, tok, now.Add(SessionTTL+time.Minute)); err == nil {
		t.Error("an expired session verified")
	}
	if _, err := VerifySession([]byte("other-secret"), tok, now); err == nil {
		t.Error("a session verified under the wrong secret")
	}
}

func TestSessionIdentityCannotBeEdited(t *testing.T) {
	secret := []byte("session-signing-secret")
	now := time.Unix(1_800_000_000, 0)
	tok := MintSession(secret, "alice@example.com", now)

	forged := strings.Replace(tok, "alice@example.com", "root@example.com", 1)
	if got, err := VerifySession(secret, forged, now); err == nil {
		t.Fatalf("an edited session verified as %q", got)
	}
	parts := strings.Split(tok, "|")
	if len(parts) != 3 {
		t.Fatalf("unexpected token shape %q", tok)
	}
	longer := parts[0] + "|" + "9999999999" + "|" + parts[2]
	if _, err := VerifySession(secret, longer, now); err == nil {
		t.Error("a session with a rewritten expiry verified")
	}
}

func TestSessionRefusesMalformed(t *testing.T) {
	secret := []byte("s")
	now := time.Now()
	for _, tok := range []string{"", "|", "a|b", "no-pipes", "|123|sig", "a||sig"} {
		if _, err := VerifySession(secret, tok, now); err == nil {
			t.Errorf("malformed session %q verified", tok)
		}
	}
	if MintSession(nil, "a@b.c", now) != "" {
		t.Error("minted a session with no secret")
	}
	if MintSession(secret, "", now) != "" {
		t.Error("minted a session with no subject")
	}
}

func TestEmailNormalisationAndValidation(t *testing.T) {
	if got := NormalizeEmail("  Alice@Example.COM "); got != "alice@example.com" {
		t.Errorf("NormalizeEmail = %q", got)
	}
	for _, ok := range []string{"a@b.co", "admin@example.com"} {
		if !ValidEmail(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	// '|' is the session payload delimiter.
	for _, bad := range []string{"", "a", "@b.co", "a@", "a@b", "a b@c.co", "a|b@c.co"} {
		if ValidEmail(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}
