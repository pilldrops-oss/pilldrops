// Package orgassert mints and verifies the signed, short-lived org assertion
// carried in X-Org-Override, bound to the node, method and path it is for.
package orgassert

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// TTL bounds how long a signed assertion is valid once minted.
const TTL = 60 * time.Second

// MaxSkew is how far a node's clock may run ahead of the control plane's and
// still accept its assertions. Without it a node two minutes fast refused every
// API call while its heartbeats kept it healthy. The heartbeat window matches
// (cluster.beatSkew), so a node skewed further is marked down, not left broken.
const MaxSkew = 2 * time.Minute

// caller is the issuer field in the payload; informational, the signature is the trust boundary.
const caller = "orchestrator"

// deriveKey domain-separates the signing key from other uses of the same shared secret.
func deriveKey(secret []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	// Old project name kept on purpose: changing it invalidates in-flight assertions.
	mac.Write([]byte("sandforge:org-override:v1"))
	return mac.Sum(nil)
}

// bindPath drops the query string: the node sees it separately from r.URL.Path,
// so binding it would make the two sides disagree.
func bindPath(path string) string {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		return path[:i]
	}
	return path
}

// bindLegacy is the pre-nonce binding (method and path only). Nothing signs
// this form any more; kept so a node still verifies assertions from a control
// plane that has not upgraded.
func bindLegacy(payload, method, path string) []byte {
	return []byte(payload + "|" + strings.ToUpper(method) + "\n" + bindPath(path))
}

// bind folds method, path and target node into the signed material without
// putting them in the payload: the node knows all three from the request, and
// binding the node id stops a node replaying a received assertion elsewhere.
//
// principal, when the control plane names the calling key (its id, then its
// role), is bound the same way and travels beside the assertion in X-Key-Id
// and X-Key-Role. Passing none appends nothing, so an assertion that names no
// key is byte-for-byte the old one: a node that never heard of key ids still
// verifies it, and a node that has cannot be handed an unsigned key id.
func bind(payload, nodeID, method, path string, principal ...string) []byte {
	b := payload + "|" + strings.ToUpper(method) + "\n" + bindPath(path) + "\n" + nodeID
	if len(principal) > 0 {
		b += "\n" + strings.Join(principal, "\n")
	}
	return []byte(b)
}

const nonceBytes = 16

// newNonce ignores rand.Read's error: since Go 1.24 it never fails.
func newNonce() string {
	var b [nonceBytes]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// Sign mints an HMAC assertion for org, valid for TTL and only for the given
// node, method and path. Returns "" when secret or org is unusable; callers
// must not relay that as an override. The body is deliberately not bound, since
// hashing it would mean buffering every upload in the node's auth middleware.
func Sign(secret []byte, org, nodeID, method, path string, now time.Time, principal ...string) string {
	if len(secret) == 0 || org == "" || strings.Contains(org, "|") {
		return ""
	}
	payload := payloadFor(org, now)
	mac := hmac.New(sha256.New, deriveKey(secret))
	mac.Write(bind(payload, nodeID, method, path, principal...))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return payload + "|" + sig
}

// Verify checks an HMAC assertion against this node's id and the request's
// method and path; any failure returns ("", "", false). Replay rejection is the
// caller's job: nonce is single-use, and "" means a legacy nonce-less token.
func Verify(secret []byte, token, nodeID, method, path string, now time.Time, principal ...string) (org, nonce string, ok bool) {
	if len(secret) == 0 || token == "" {
		return "", "", false
	}
	payload, sigB64, gotOrg, gotNonce, exp, ok := splitToken(token)
	if !ok {
		return "", "", false
	}
	mac := hmac.New(sha256.New, deriveKey(secret))
	mac.Write(bindFor(payload, gotNonce, nodeID, method, path, principal...))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(sigB64), []byte(want)) != 1 {
		return "", "", false
	}
	if now.Add(-MaxSkew).Unix() > exp {
		return "", "", false
	}
	return gotOrg, gotNonce, true
}

// bindFor picks the legacy binding for nonce-less tokens. Accepting both is
// safe because the same key signs both shapes; the legacy one is merely
// replayable within TTL, and PILLDROPS_REQUIRE_SECURE_HOP lets a node refuse it.
func bindFor(payload, nonce, nodeID, method, path string, principal ...string) []byte {
	if nonce == "" {
		return bindLegacy(payload, method, path)
	}
	return bind(payload, nodeID, method, path, principal...)
}

// SignAsym mints an Ed25519 assertion. Unlike HMAC, a node holding only the
// public key cannot mint one, so a compromised node cannot act as other tenants.
// Returns "" for a bad key or unusable org, like Sign.
func SignAsym(priv ed25519.PrivateKey, org, nodeID, method, path string, now time.Time, principal ...string) string {
	if len(priv) != ed25519.PrivateKeySize || org == "" || strings.Contains(org, "|") {
		return ""
	}
	payload := payloadFor(org, now)
	sig := ed25519.Sign(priv, bind(payload, nodeID, method, path, principal...))
	return payload + "|" + base64.RawURLEncoding.EncodeToString(sig)
}

// VerifyAsym checks an Ed25519 assertion against the control plane's public key.
// A node with a public key configured must accept only this form, never HMAC too.
func VerifyAsym(pub ed25519.PublicKey, token, nodeID, method, path string, now time.Time, principal ...string) (org, nonce string, ok bool) {
	if len(pub) != ed25519.PublicKeySize || token == "" {
		return "", "", false
	}
	payload, sigB64, gotOrg, gotNonce, exp, ok := splitToken(token)
	if !ok {
		return "", "", false
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return "", "", false
	}
	if !ed25519.Verify(pub, bindFor(payload, gotNonce, nodeID, method, path, principal...), sig) {
		return "", "", false
	}
	if now.Add(-MaxSkew).Unix() > exp {
		return "", "", false
	}
	return gotOrg, gotNonce, true
}

// payloadFor builds the visible signed prefix "org|caller|exp|nonce". The nonce
// travels in the clear because the node must read it for replay checks.
func payloadFor(org string, now time.Time) string {
	return org + "|" + caller + "|" + strconv.FormatInt(now.Add(TTL).Unix(), 10) + "|" + newNonce()
}

// splitToken parses "org|caller|exp|nonce|sig" or the legacy "org|caller|exp|sig"
// (nonce == ""), shared by both schemes so their parsing cannot drift. Field
// count is unambiguous because org may not contain "|".
func splitToken(token string) (payload, sig, org, nonce string, exp int64, ok bool) {
	i := strings.LastIndexByte(token, '|')
	if i < 0 {
		return "", "", "", "", 0, false
	}
	payload, sig = token[:i], token[i+1:]
	parts := strings.Split(payload, "|")
	if len(parts) != 3 && len(parts) != 4 {
		return "", "", "", "", 0, false
	}
	if parts[0] == "" || parts[1] != caller {
		return "", "", "", "", 0, false
	}
	exp, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return "", "", "", "", 0, false
	}
	if len(parts) == 4 {
		if parts[3] == "" {
			// An empty nonce field would verify under the legacy binding while looking current.
			return "", "", "", "", 0, false
		}
		nonce = parts[3]
	}
	return payload, sig, parts[0], nonce, exp, true
}

// ParsePrivateKey reads a 32-byte hex seed (what `openssl rand -hex 32` produces).
// Returns nil, nil for an empty string.
func ParsePrivateKey(hexSeed string) (ed25519.PrivateKey, error) {
	s := strings.TrimSpace(hexSeed)
	if s == "" {
		return nil, nil
	}
	seed, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("ORG_ASSERT_KEY: not valid hex: %w", err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("ORG_ASSERT_KEY must be %d bytes (%d hex chars), got %d",
			ed25519.SeedSize, ed25519.SeedSize*2, len(seed))
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// ParsePublicKey reads a 32-byte hex public key. Returns nil, nil for an empty string.
func ParsePublicKey(hexKey string) (ed25519.PublicKey, error) {
	s := strings.TrimSpace(hexKey)
	if s == "" {
		return nil, nil
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("ORG_ASSERT_PUBLIC_KEY: not valid hex: %w", err)
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("ORG_ASSERT_PUBLIC_KEY must be %d bytes (%d hex chars), got %d",
			ed25519.PublicKeySize, ed25519.PublicKeySize*2, len(b))
	}
	return ed25519.PublicKey(b), nil
}

// PublicKeyHex renders a private key's public half for delivery to nodes.
func PublicKeyHex(priv ed25519.PrivateKey) string {
	if len(priv) != ed25519.PrivateKeySize {
		return ""
	}
	return hex.EncodeToString(priv.Public().(ed25519.PublicKey))
}
