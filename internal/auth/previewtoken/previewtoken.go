// Package previewtoken mints and verifies the two preview credentials: the
// node hop (orchestrator to node, bound to id, port and org) and the browser
// hop (share link and pdpv_ cookie, bound to id, port and expiry).
package previewtoken

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"strconv"
	"strings"
	"time"
)

// Token mints the HMAC node-hop credential for id's port, scoped to org.
// org may be "" when the minter cannot resolve the owner; returns "" when secret or id is empty.
func Token(secret []byte, id string, port int, org string) string {
	if len(secret) == 0 || id == "" {
		return ""
	}
	mac := hmac.New(sha256.New, deriveKey(secret))
	mac.Write([]byte(id + "|" + strconv.Itoa(port) + "|" + org))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))[:22]
}

// Valid reports, in constant time, whether token authorizes org to reach id's port.
// Always false for an empty secret or token: an unconfigured secret validates nothing.
func Valid(secret []byte, token, id string, port int, org string) bool {
	if len(secret) == 0 || token == "" {
		return false
	}
	want := Token(secret, id, port, org)
	if want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(want)) == 1
}

// BrowserKey derives the browser-hop HMAC key. The "pdpv:" prefix keeps this
// construction distinct from the node hop even when both use the same secret.
func BrowserKey(secret string) []byte { return []byte("pdpv:" + secret) }

// Browser mints the browser-hop credential for one port of one sandbox, shape
// "<unix-expiry base36>.<22-char mac>". The org is deliberately not bound
// because the portal mints without store access. There is no dual-accept for
// the retired id-only or unexpiring forms.
//
// gen is the revocation generation: a short string the verifier re-reads from
// the sandbox on every request, so a token stops validating the moment that
// string changes. The orchestrator passes the sandbox's current visibility
// (internal/orchestrator/preview.go), which is what makes "make this private"
// retract the links already handed out instead of only closing the API. Bound
// last, so a gen that contains the field separator cannot shift the fields
// before it.
func Browser(key []byte, id string, port int, gen string) string {
	return BrowserAt(key, id, port, gen, time.Now().Add(BrowserTTL))
}

// BrowserTTL matches the pdpv_ cookie MaxAge.
const BrowserTTL = 24 * time.Hour

// browserSkew tolerates NTP drift between the minting and verifying replicas.
const browserSkew = 60 * time.Second

// BrowserAt is Browser with an explicit expiry.
func BrowserAt(key []byte, id string, port int, gen string, exp time.Time) string {
	if len(key) == 0 || id == "" || port <= 0 || exp.IsZero() {
		return ""
	}
	expS := strconv.FormatInt(exp.Unix(), 36)
	return expS + "." + browserMAC(key, id, port, gen, expS)
}

// browserMAC signs id, port, expiry and generation together so a holder can
// edit none of them.
func browserMAC(key []byte, id string, port int, gen, expS string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "|" + strconv.Itoa(port) + "|" + expS + "|" + gen))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))[:22]
}

// BrowserValid reports, in constant time, whether token authorizes id's port
// now under generation gen. Always false for an empty key or token, or for a
// token minted under a different generation.
func BrowserValid(key []byte, token, id string, port int, gen string) bool {
	return BrowserValidAt(key, token, id, port, gen, time.Now())
}

// BrowserValidAt is BrowserValid against a supplied clock. The MAC is checked
// before the expiry is trusted.
func BrowserValidAt(key []byte, token, id string, port int, gen string, now time.Time) bool {
	if len(key) == 0 || token == "" {
		return false
	}
	dot := strings.IndexByte(token, '.')
	if dot <= 0 || dot == len(token)-1 {
		// Covers the retired unexpiring form too; deliberately not distinguished from junk.
		return false
	}
	expS, sig := token[:dot], token[dot+1:]
	want := browserMAC(key, id, port, gen, expS)
	if want == "" || subtle.ConstantTimeCompare([]byte(sig), []byte(want)) != 1 {
		return false
	}
	exp, err := strconv.ParseInt(expS, 36, 64)
	if err != nil {
		return false
	}
	return !now.Add(-browserSkew).After(time.Unix(exp, 0))
}

// deriveKey domain-separates the node-hop key from orgassert's use of the same secret.
func deriveKey(secret []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	// Old project name kept on purpose: changing it invalidates every issued token.
	mac.Write([]byte("sandforge:preview:v1"))
	return mac.Sum(nil)
}

// Authorized is the shared node-side preview gate for both the path and
// subdomain mounts. An unconfigured secret admits only when devInsecure, which
// the caller evaluates. Accepts an org-bound token or the org-blind form.
func Authorized(secret []byte, token, id string, port int, org string, devInsecure bool) bool {
	if len(secret) == 0 {
		return devInsecure
	}
	return Valid(secret, token, id, port, org) || Valid(secret, token, id, port, "")
}

// TokenAsym mints the Ed25519 node-hop credential, shape "<unix-expiry base36>.<base64 sig>".
// Fleet nodes verify with the public key so a compromised node cannot mint for
// another node's sandboxes; a standalone node still uses the HMAC form.
func TokenAsym(priv ed25519.PrivateKey, id string, port int, org string, exp time.Time) string {
	if len(priv) != ed25519.PrivateKeySize || id == "" || exp.IsZero() {
		return ""
	}
	expS := strconv.FormatInt(exp.Unix(), 36)
	sig := ed25519.Sign(priv, previewMsg(id, port, org, expS))
	return expS + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// AuthorizedAsym verifies a TokenAsym credential; like Authorized it accepts the org-blind form.
func AuthorizedAsym(pub ed25519.PublicKey, token, id string, port int, org string, now time.Time) bool {
	if len(pub) != ed25519.PublicKeySize || token == "" {
		return false
	}
	dot := strings.IndexByte(token, '.')
	if dot <= 0 || dot == len(token)-1 {
		return false
	}
	expS, sigB64 := token[:dot], token[dot+1:]
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return false
	}
	if !ed25519.Verify(pub, previewMsg(id, port, org, expS), sig) &&
		!ed25519.Verify(pub, previewMsg(id, port, "", expS), sig) {
		return false
	}
	exp, err := strconv.ParseInt(expS, 36, 64)
	if err != nil {
		return false
	}
	return !now.After(time.Unix(exp, 0))
}

// previewMsg domain-separates this signature from orgassert's, which shares the key.
func previewMsg(id string, port int, org, expS string) []byte {
	return []byte("sandforge:preview-hop:v1|" + id + "|" + strconv.Itoa(port) + "|" + org + "|" + expS)
}
