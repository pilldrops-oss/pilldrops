package previewtoken

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

// legacyBrowserToken is the retired id-only construction the current one must differ from.
func legacyBrowserToken(secret []byte, id string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(id))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))[:22]
}

func TestPortBindingActuallyChangedTheToken(t *testing.T) {
	const id = "sb-1755600000000-a1b2c3d4"
	for _, secret := range []string{"orch-key", "another-key-entirely", "k"} {
		key := BrowserKey(secret)
		if got, old := Browser(key, id, 3000, "org"), legacyBrowserToken(key, id); got == old {
			t.Errorf("port-bound token equals the id-only token %q — the port is not in the MAC", old)
		}
	}
}

func TestEachPortGetsADistinctToken(t *testing.T) {
	key := BrowserKey("orch-key")
	const id = "sb-1755600000000-a1b2c3d4"

	tok3000 := Browser(key, id, 3000, "org")
	if tok3000 == "" {
		t.Fatal("no token minted")
	}
	if Browser(key, id, 8080, "org") == tok3000 {
		t.Fatal("ports 3000 and 8080 produce the SAME token")
	}
	if !BrowserValid(key, tok3000, id, 3000, "org") {
		t.Error("the token did not validate for its own port")
	}
	if BrowserValid(key, tok3000, id, 8080, "org") {
		t.Error("a :3000 token opened :8080 — this is the cross-port reach the port binding removes")
	}
	if BrowserValid(key, tok3000, id, 30000, "org") {
		t.Error("a :3000 token opened :30000 — the port is being prefix-matched")
	}
}

// TestBrowserKeyIsThePdpvPrefix pins the prefix outside Browser: devserver with
// PREVIEW_SECRET passes raw bytes and never calls BrowserKey.
func TestBrowserKeyIsThePdpvPrefix(t *testing.T) {
	if got, want := string(BrowserKey("abc")), "pdpv:abc"; got != want {
		t.Errorf("BrowserKey(%q) = %q, want %q", "abc", got, want)
	}
	raw := []byte("purpose-built-secret")
	if Browser(raw, "sb1", 3000, "org") == Browser(BrowserKey("purpose-built-secret"), "sb1", 3000, "org") {
		t.Error("Browser applied BrowserKey to a raw secret: PREVIEW_SECRET and the fallback key would collide")
	}
}

// TestBrowserAndNodeTokensDoNotCrossValidate forces the same secret into both
// hops so only the key derivation keeps them apart.
func TestBrowserAndNodeTokensDoNotCrossValidate(t *testing.T) {
	const (
		shared = "one-secret-for-both"
		id     = "sb-1755600000000-a1b2c3d4"
		port   = 3000
		org    = "acme"
	)
	browserTok := Browser(BrowserKey(shared), id, port, "org")
	nodeTok := Token([]byte(shared), id, port, org)

	if browserTok == "" || nodeTok == "" {
		t.Fatal("fixture produced an empty token")
	}
	if browserTok == nodeTok {
		t.Fatal("browser and node tokens are identical under a shared secret — there is no audience separation")
	}
	if Authorized([]byte(shared), browserTok, id, port, org, false) {
		t.Error("a BROWSER token authorized a NODE preview request — a share-link recipient could reach the node's proxy directly")
	}
	if BrowserValid(BrowserKey(shared), nodeTok, id, port, "org") {
		t.Error("a NODE token validated as a browser token")
	}
}

func TestBrowserValidRejectsTheObviousForgeries(t *testing.T) {
	key := BrowserKey("orch-key")
	const id = "sb-1755600000000-a1b2c3d4"
	good := Browser(key, id, 3000, "org")

	if !BrowserValid(key, good, id, 3000, "org") {
		t.Fatal("the genuine token did not validate")
	}
	for _, tc := range []struct{ name, token, id string }{
		{"empty token", "", id},
		{"token for another sandbox", Browser(key, "sb-other", 3000, "org"), id},
		{"truncated", good[:len(good)-1], id},
		{"one byte flipped", flip(good), id},
		{"right token, wrong sandbox", good, "sb-other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if BrowserValid(key, tc.token, tc.id, 3000, "org") {
				t.Errorf("BrowserValid accepted %s", tc.name)
			}
		})
	}
	if BrowserValid(nil, good, id, 3000, "org") {
		t.Error("an empty key validated a token: the gate would be off, not closed")
	}
	if Browser(nil, id, 3000, "org") != "" || Browser(key, "", 3000, "org") != "" || Browser(key, id, 0, "org") != "" {
		t.Error("Browser returned a token for a degenerate identity")
	}
}

func flip(s string) string {
	b := []byte(s)
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	return string(b)
}
