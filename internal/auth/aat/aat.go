// Package aat implements Agent Access Tokens, sfa_<id>_<secret>: revocable,
// sandbox-bound, scoped credentials of which only the SHA-256 is stored.
// No in-guest code must import this package: a sandbox cannot mint its own credentials.
package aat

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Prefix marks an Agent Access Token; distinct from sf_live_ so leak scanners can match it.
const Prefix = "sfa_"

const secretBytes = 32

// ErrMalformed is the only parse error callers see. Verify answers a bool, so
// a caller cannot tell a bad id from a bad secret and cannot probe for valid ids.
var ErrMalformed = errors.New("aat: malformed token")

// Scope is one permission. Scopes do not imply each other.
type Scope string

const (
	// ScopeRead sees status, turn history and the event stream.
	ScopeRead Scope = "agent:read"

	// ScopeWrite submits and interrupts turns. It does not include console.
	ScopeWrite Scope = "agent:write"

	// ScopeConsole opens an interactive terminal. Auto-revoked when the sandbox pauses.
	ScopeConsole Scope = "agent:console"

	// ScopeApprove answers the agent's approval prompts.
	ScopeApprove Scope = "agent:approve"

	// ScopeFiles reads and writes the workspace.
	ScopeFiles Scope = "agent:files"

	// ScopeAdmin manages the sandbox's agent config. It cannot mint or revoke tokens.
	ScopeAdmin Scope = "agent:admin"
)

// All is every scope, least to most dangerous.
var All = []Scope{ScopeRead, ScopeWrite, ScopeApprove, ScopeFiles, ScopeAdmin, ScopeConsole}

// Describe returns a one-line explanation of what granting a scope means.
func Describe(s Scope) string {
	switch s {
	case ScopeRead:
		return "See the agent's status, task history and live output."
	case ScopeWrite:
		return "Give the agent new tasks and stop running ones."
	case ScopeConsole:
		return "Open a terminal and run any command inside the sandbox."
	case ScopeApprove:
		return "Approve or deny the actions the agent asks permission for."
	case ScopeFiles:
		return "Read and change files in the workspace."
	case ScopeAdmin:
		return "Change the agent's model, budget and environment."
	}
	return string(s)
}

// Valid reports whether s is a scope this build knows.
func Valid(s Scope) bool {
	return slices.Contains(All, s)
}

// Set is a scope set with the subset checks the HTTP layer needs.
type Set map[Scope]struct{}

// NewSet builds a set, rejecting unknown scopes.
func NewSet(scopes []Scope) (Set, error) {
	out := make(Set, len(scopes))
	for _, s := range scopes {
		if !Valid(s) {
			return nil, fmt.Errorf("aat: unknown scope %q", string(s))
		}
		out[s] = struct{}{}
	}
	if len(out) == 0 {
		return nil, errors.New("aat: a token needs at least one scope")
	}
	return out, nil
}

// Has reports whether the set grants scope. No scope implies another.
func (s Set) Has(scope Scope) bool {
	_, ok := s[scope]
	return ok
}

// HasAny reports whether the set grants at least one of scopes.
func (s Set) HasAny(scopes ...Scope) bool {
	for _, sc := range scopes {
		if s.Has(sc) {
			return true
		}
	}
	return false
}

// Sorted returns the scopes in a stable order, for storage and display.
func (s Set) Sorted() []Scope {
	out := make([]Scope, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Encode serialises a set for the scopes column: space-separated, OAuth-style.
func (s Set) Encode() string {
	parts := make([]string, 0, len(s))
	for _, sc := range s.Sorted() {
		parts = append(parts, string(sc))
	}
	return strings.Join(parts, " ")
}

// DecodeSet parses a stored scopes column. Unknown entries are dropped, not
// errors, so a rollback or mixed-version fleet does not lock every token out.
func DecodeSet(encoded string) Set {
	out := Set{}
	for _, f := range strings.Fields(encoded) {
		if sc := Scope(f); Valid(sc) {
			out[sc] = struct{}{}
		}
	}
	return out
}

// Token is a freshly minted credential. Nothing persists Secret.
type Token struct {
	// ID is the public half, safe to log and display.
	ID string
	// Secret is the plaintext token, shown exactly once.
	Secret string
	// SecretSHA256 is what the database stores.
	SecretSHA256 string
}

// Mint creates a new token.
func Mint() Token {
	id := randHex(6)
	secret := Prefix + id + "_" + randHex(secretBytes)
	return Token{ID: id, Secret: secret, SecretSHA256: HashHex(secret)}
}

// HashHex is the lower-case hex SHA-256 of a whole token string, so the id is
// authenticated by the hash too.
func HashHex(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ParseID extracts the public id from a token string without verifying anything.
func ParseID(token string) (string, error) {
	if !strings.HasPrefix(token, Prefix) {
		return "", ErrMalformed
	}
	rest := token[len(Prefix):]
	i := strings.IndexByte(rest, '_')
	if i <= 0 || i == len(rest)-1 {
		return "", ErrMalformed
	}
	id := rest[:i]
	if len(rest)-i-1 < 16 {
		return "", ErrMalformed
	}
	if !isHex(id) {
		return "", ErrMalformed
	}
	return id, nil
}

// VerifyHash reports whether token matches expectedHash, in constant time.
func VerifyHash(token, expectedHash string) bool {
	if token == "" || expectedHash == "" {
		return false
	}
	got := HashHex(token)
	return subtle.ConstantTimeCompare([]byte(got), []byte(expectedHash)) == 1
}

// RandomCode returns a single-use, URL-safe code with 128 bits of entropy.
func RandomCode() string { return randHex(16) }

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// A predictable token would be a forgeable credential; panic is correct here.
		panic("aat: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return len(s) > 0
}
