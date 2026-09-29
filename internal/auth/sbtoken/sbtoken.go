// Package sbtoken mints and verifies the per-sandbox envd access token. Only
// the SHA-256 hash is shipped into the guest; the token itself never is.
package sbtoken

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
)

// MetadataKey is the gRPC metadata / header key carrying the token.
const MetadataKey = "x-access-token"

// IdentityPrefix marks a sandbox's own platform credential (Mint output with
// this prefix). The control plane routes bearers with it to the sandbox row
// rather than the API-key table; cmd/envd validates the same literal.
const IdentityPrefix = "pd_sb_"

// Mint returns a fresh 256-bit token (hex) and its SHA-256 hash (hex).
func Mint() (token, hash string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	token = hex.EncodeToString(b)
	return token, HashHex(token)
}

// HashHex is the lower-case hex SHA-256 of a token.
func HashHex(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Verify reports whether token hashes to expectedHash, in constant time. Returns
// false if either side is empty so a missing token never authenticates.
func Verify(token, expectedHash string) bool {
	if token == "" || expectedHash == "" {
		return false
	}
	got := HashHex(token)
	return subtle.ConstantTimeCompare([]byte(got), []byte(expectedHash)) == 1
}
