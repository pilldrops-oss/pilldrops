package orgassert

import (
	"strings"
	"testing"
	"time"
)

func TestSignVerifyRoundTrip(t *testing.T) {
	secret := []byte("shared-secret")
	now := time.Now()
	tok := Sign(secret, "u:cust9", "node-a", "GET", "/v1/sandboxes", now)
	if tok == "" {
		t.Fatal("Sign returned empty token for a valid secret/org")
	}
	org, _, ok := Verify(secret, tok, "node-a", "GET", "/v1/sandboxes", now)
	if !ok || org != "u:cust9" {
		t.Fatalf("Verify(fresh token) = (%q, %v), want (u:cust9, true)", org, ok)
	}
	// Still valid just under the TTL boundary.
	if org, _, ok := Verify(secret, tok, "node-a", "GET", "/v1/sandboxes", now.Add(TTL-time.Second)); !ok || org != "u:cust9" {
		t.Fatalf("Verify(just-under-TTL) = (%q, %v), want (u:cust9, true)", org, ok)
	}
}

func TestSignEmptyInputsRefuse(t *testing.T) {
	if tok := Sign(nil, "u:cust9", "node-a", "GET", "/v1/sandboxes", time.Now()); tok != "" {
		t.Errorf("Sign with no secret = %q, want empty", tok)
	}
	if tok := Sign([]byte("k"), "", "node-a", "GET", "/v1/sandboxes", time.Now()); tok != "" {
		t.Errorf("Sign with no org = %q, want empty", tok)
	}
	// A "|" in the org would let the payload reparse as a different org|caller|expiry triple.
	if tok := Sign([]byte("k"), "u:cust9|orchestrator|9999999999", "node-a", "GET", "/v1/sandboxes", time.Now()); tok != "" {
		t.Errorf("Sign with a '|'-containing org = %q, want empty (refused)", tok)
	}
}

func TestVerifyRejectsTamperedOrg(t *testing.T) {
	secret := []byte("shared-secret")
	now := time.Now()
	tok := Sign(secret, "u:cust9", "node-a", "GET", "/v1/sandboxes", now)
	tampered := strings.Replace(tok, "u:cust9", "u:victim", 1)
	if org, _, ok := Verify(secret, tampered, "node-a", "GET", "/v1/sandboxes", now); ok {
		t.Fatalf("tampered org verified as %q, want rejected", org)
	}
}

func TestVerifyRejectsWrongSecret(t *testing.T) {
	tok := Sign([]byte("real-secret"), "u:cust9", "node-a", "GET", "/v1/sandboxes", time.Now())
	if org, _, ok := Verify([]byte("guessed-secret"), tok, "node-a", "GET", "/v1/sandboxes", time.Now()); ok {
		t.Fatalf("forged/wrong-secret token verified as %q, want rejected", org)
	}
}

func TestVerifyRejectsExpired(t *testing.T) {
	secret := []byte("shared-secret")
	now := time.Now()
	tok := Sign(secret, "u:cust9", "node-a", "GET", "/v1/sandboxes", now)
	if org, _, ok := Verify(secret, tok, "node-a", "GET", "/v1/sandboxes", now.Add(TTL+MaxSkew+time.Second)); ok {
		t.Fatalf("expired token verified as %q, want rejected", org)
	}
}

func TestVerifyRejectsMalformedAndEmpty(t *testing.T) {
	secret := []byte("shared-secret")
	now := time.Now()
	cases := []string{
		"",
		"not-a-token-at-all",
		"u:cust9|orchestrator|not-a-number|deadbeef",
		"u:cust9|wrong-caller|9999999999|deadbeef",
	}
	for _, tok := range cases {
		if org, _, ok := Verify(secret, tok, "node-a", "GET", "/v1/sandboxes", now); ok {
			t.Errorf("Verify(%q) = (%q, true), want rejected", tok, org)
		}
	}
	// A validly signed token must not verify against an empty secret.
	real := Sign([]byte("some-secret"), "u:cust9", "node-a", "GET", "/v1/sandboxes", now)
	if org, _, ok := Verify(nil, real, "node-a", "GET", "/v1/sandboxes", now); ok {
		t.Fatalf("Verify with empty secret = (%q, true), want rejected", org)
	}
}

func TestSignDeterministicPerSecondNotGuessable(t *testing.T) {
	now := time.Now()
	a := Sign([]byte("secret-a"), "u:cust9", "node-a", "GET", "/v1/sandboxes", now)
	b := Sign([]byte("secret-b"), "u:cust9", "node-a", "GET", "/v1/sandboxes", now)
	if a == b {
		t.Fatal("Sign produced identical tokens under different secrets")
	}
}

func TestAnAssertionDoesNotVerifyForADifferentOperation(t *testing.T) {
	secret := []byte("shared-secret")
	now := time.Now()
	tok := Sign(secret, "u:cust9", "node-a", "GET", "/v1/sandboxes", now)

	if org, _, ok := Verify(secret, tok, "node-a", "GET", "/v1/sandboxes", now); !ok || org != "u:cust9" {
		t.Fatalf("the assertion did not verify for its own operation: (%q, %v)", org, ok)
	}
	for _, tc := range []struct{ name, method, path string }{
		{"a different method on the same path", "DELETE", "/v1/sandboxes"},
		{"the destructive route this could escalate to", "DELETE", "/v1/sandboxes/sb-victim"},
		{"a different path", "GET", "/v1/limits"},
		{"a sibling under the same prefix", "GET", "/v1/sandboxes/sb-victim"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if org, _, ok := Verify(secret, tok, "node-a", tc.method, tc.path, now); ok {
				t.Fatalf("an assertion minted for GET /v1/sandboxes verified for %s %s as %q",
					tc.method, tc.path, org)
			}
		})
	}
}

func TestMethodBindingIsCaseInsensitive(t *testing.T) {
	secret := []byte("shared-secret")
	now := time.Now()
	tok := Sign(secret, "u:cust9", "node-a", "get", "/v1/sandboxes", now)
	if _, _, ok := Verify(secret, tok, "node-a", "GET", "/v1/sandboxes", now); !ok {
		t.Fatal("an assertion signed for \"get\" did not verify for \"GET\"")
	}
}

func TestTheQueryStringIsNotPartOfTheBinding(t *testing.T) {
	secret := []byte("shared-secret")
	now := time.Now()
	tok := Sign(secret, "u:cust9", "node-a", "POST", "/v1/images/build?tag=x&f=1", now)
	if _, _, ok := Verify(secret, tok, "node-a", "POST", "/v1/images/build", now); !ok {
		t.Fatal("an assertion signed with a query did not verify against the bare path")
	}
}

// A node whose clock runs ahead by less than MaxSkew still accepts a fresh
// assertion; this is what a skewed node refused before.
func TestVerifyToleratesANodeClockAhead(t *testing.T) {
	secret := []byte("shared-secret")
	now := time.Now()
	tok := Sign(secret, "u:cust9", "node-a", "GET", "/v1/sandboxes", now)
	if _, _, ok := Verify(secret, tok, "node-a", "GET", "/v1/sandboxes", now.Add(TTL+MaxSkew-time.Second)); !ok {
		t.Fatal("a node less than MaxSkew ahead refused a fresh assertion")
	}
}
