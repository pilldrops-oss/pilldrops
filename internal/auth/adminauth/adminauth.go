// Package adminauth stores and verifies the operator console's passwords and
// mints the session cookie a login exchanges them for. The API key still
// authenticates the API; this only decides who may open the console.
package adminauth

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Iterations is the PBKDF2-HMAC-SHA256 work factor (OWASP floor; stdlib, no
// x/crypto dependency).
const Iterations = 600_000

const (
	saltLen = 16
	keyLen  = 32
)

// MinPasswordLen is a length floor only; composition rules are deliberately absent.
const MinPasswordLen = 12

// ErrWeakPassword is returned for a password shorter than MinPasswordLen.
var ErrWeakPassword = fmt.Errorf("password must be at least %d characters", MinPasswordLen)

// Hash derives a verifier "pbkdf2$<iterations>$<salt>$<key>". It carries its
// own parameters so raising Iterations does not invalidate stored hashes.
func Hash(password string) (string, error) {
	if len(password) < MinPasswordLen {
		return "", ErrWeakPassword
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("read salt: %w", err)
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, Iterations, keyLen)
	if err != nil {
		return "", fmt.Errorf("derive: %w", err)
	}
	return "pbkdf2$" + strconv.Itoa(Iterations) + "$" +
		hex.EncodeToString(salt) + "$" + hex.EncodeToString(key), nil
}

// Verify reports whether password matches the stored hash, in constant time.
// A malformed stored hash is false, never an error.
func Verify(stored, password string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false
	}
	salt, err := hex.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := hex.DecodeString(parts[3])
	if err != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// NeedsUpgrade reports whether a stored hash has a lower work factor than
// Iterations, so a login can re-hash while the plaintext is available.
func NeedsUpgrade(stored string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2" {
		return true
	}
	iter, err := strconv.Atoi(parts[1])
	return err != nil || iter < Iterations
}

// SessionTTL bounds a console session.
const SessionTTL = 12 * time.Hour

// ErrSession is returned for every session failure so an attacker cannot tell them apart.
var ErrSession = errors.New("invalid or expired session")

// MintSession returns a signed, self-describing session token carrying the
// email and expiry; there is no server-side session table.
func MintSession(secret []byte, email string, now time.Time) string {
	if len(secret) == 0 || email == "" {
		return ""
	}
	exp := now.Add(SessionTTL).Unix()
	payload := email + "|" + strconv.FormatInt(exp, 10)
	return payload + "|" + base64.RawURLEncoding.EncodeToString(sign(secret, payload))
}

// VerifySession returns the email a token attests to.
func VerifySession(secret []byte, token string, now time.Time) (string, error) {
	if len(secret) == 0 || token == "" {
		return "", ErrSession
	}
	i := strings.LastIndexByte(token, '|')
	if i < 0 {
		return "", ErrSession
	}
	payload, sig := token[:i], token[i+1:]
	parts := strings.SplitN(payload, "|", 2)
	if len(parts) != 2 || parts[0] == "" {
		return "", ErrSession
	}
	// Check the MAC before trusting the expiry.
	want := base64.RawURLEncoding.EncodeToString(sign(secret, payload))
	if subtle.ConstantTimeCompare([]byte(sig), []byte(want)) != 1 {
		return "", ErrSession
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || now.Unix() > exp {
		return "", ErrSession
	}
	return parts[0], nil
}

// sign domain-separates the session MAC from other uses of the same secret.
func sign(secret []byte, payload string) []byte {
	k := hmac.New(sha256.New, secret)
	k.Write([]byte("pilldrops:console-session:v1"))
	mac := hmac.New(sha256.New, k.Sum(nil))
	mac.Write([]byte(payload))
	return mac.Sum(nil)
}

// NormalizeEmail lowercases and trims so case variants are one account.
func NormalizeEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// ValidEmail is a loose typo check, not an RFC 5322 parser. It rejects '|',
// the session payload delimiter.
func ValidEmail(s string) bool {
	at := strings.IndexByte(s, '@')
	return at > 0 && at < len(s)-1 &&
		!strings.ContainsAny(s, " \t\r\n|") &&
		strings.Contains(s[at+1:], ".")
}
