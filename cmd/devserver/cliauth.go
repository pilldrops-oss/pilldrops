// Credential identity for the portal API.
//
// There used to be a second key store here — CLIKeyStore, minted by a browser
// login flow at /cli/auth. Nothing ever served that route, so Mint had no
// caller, nothing could write cli_keys.json, and Lookup validated a file that
// could not exist. It has been deleted rather than wired: Mint took a user id
// and an email, and this portal has no identity provider to supply either (see
// ownerID in main.go). Keys are minted by POST /v1/keys, which is real,
// org-scoped, and what `pilldrops keys create` drives.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
)

// hashToken is how a key is stored: the state file never holds a usable
// credential, only the hash of one.
func hashToken(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

// apiKeyOwner is the fallback owner for a token with no bound owner. Callers
// try APIKeyStore.OwnerOf first, so an unbound token resolves to the same
// isolated owner everywhere.
func apiKeyOwner(token string) string {
	h := sha256.Sum256([]byte(token))
	return "k:" + hex.EncodeToString(h[:])[:12]
}

// requestKeyID names the credential a request arrived with, which is the
// portal's member below the org: the same derivation as apiKeyOwner, because a
// bare token's owner and its key are the same thing here. "" means the request
// carried no credential and so creates nothing anyone owns.
//
// Derived, not looked up: it must be the same value on every hop and after a
// portal restart, and the key store holds hashes of exactly this token.
func requestKeyID(r *http.Request) string {
	tok := presentedToken(r)
	if tok == "" {
		return ""
	}
	return apiKeyOwner(tok)
}

// presentedToken is the credential a request carries: a bearer header, an
// X-API-Key header, or the portal session cookie the browser dashboard sends.
// "" means the request is anonymous.
func presentedToken(r *http.Request) string {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" {
		tok = r.Header.Get("X-API-Key")
	}
	if tok == "" {
		if c, err := r.Cookie(portalCookie); err == nil {
			tok = c.Value
		}
	}
	return tok
}
