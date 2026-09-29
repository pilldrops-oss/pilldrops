package previewtoken

import (
	"strings"
	"testing"
	"time"
)

func TestABrowserTokenStopsWorkingAfterItsExpiry(t *testing.T) {
	key := BrowserKey("s3cret")
	now := time.Unix(1_800_000_000, 0)
	tok := BrowserAt(key, "sb1", 3000, "org", now.Add(time.Hour))

	if !BrowserValidAt(key, tok, "sb1", 3000, "org", now) {
		t.Fatal("a fresh token was rejected")
	}
	if !BrowserValidAt(key, tok, "sb1", 3000, "org", now.Add(59*time.Minute)) {
		t.Error("rejected inside its window")
	}
	// Past expiry + the skew allowance.
	if BrowserValidAt(key, tok, "sb1", 3000, "org", now.Add(time.Hour+browserSkew+time.Second)) {
		t.Error("an EXPIRED token still validates — the expiry is decorative")
	}
}

func TestTheExpiryCannotBeExtendedByEditingTheToken(t *testing.T) {
	key := BrowserKey("s3cret")
	now := time.Unix(1_800_000_000, 0)
	tok := BrowserAt(key, "sb1", 3000, "org", now.Add(time.Minute))

	dot := strings.IndexByte(tok, '.')
	if dot <= 0 {
		t.Fatalf("token %q has no expiry segment", tok)
	}
	sig := tok[dot+1:]
	// Same signature, an expiry a decade later.
	forged := BrowserAt(key, "sb1", 3000, "org", now.Add(10*365*24*time.Hour))
	forged = forged[:strings.IndexByte(forged, '.')+1] + sig

	if BrowserValidAt(key, forged, "sb1", 3000, "org", now.Add(time.Hour)) {
		t.Fatal("a token with a rewritten expiry validated — the expiry is not covered by the MAC")
	}
}

func TestTheRetiredUnexpiringFormIsRefused(t *testing.T) {
	key := BrowserKey("s3cret")
	// The old shape: a bare 22-char MAC over "id|port" with no expiry segment.
	legacy := browserMAC(key, "sb1", 3000, "org", "")
	if strings.Contains(legacy, ".") {
		t.Fatalf("test built the wrong shape: %q", legacy)
	}
	if BrowserValid(key, legacy, "sb1", 3000, "org") {
		t.Fatal("the unexpiring legacy token still validates — immortal links survive the fix")
	}
}

func TestMalformedBrowserTokensAreRefused(t *testing.T) {
	key := BrowserKey("s3cret")
	for _, tok := range []string{
		"", ".", "abc.", ".abc", "notbase36.AAAAAAAAAAAAAAAAAAAAAA",
		"zzzzzzzzzzzzzzzzzzzz.AAAAAAAAAAAAAAAAAAAAAA", "....",
	} {
		if BrowserValid(key, tok, "sb1", 3000, "org") {
			t.Errorf("malformed token %q validated", tok)
		}
	}
	if BrowserValidAt(nil, BrowserAt(BrowserKey("k"), "sb1", 3000, "org", time.Now().Add(time.Hour)), "sb1", 3000, "org", time.Now()) {
		t.Error("an unconfigured key validated a token")
	}
}
