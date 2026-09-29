package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/auth/aat"
)

func readSet(t *testing.T, scopes ...aat.Scope) aat.Set {
	t.Helper()
	set, err := aat.NewSet(scopes)
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	return set
}

func mintFor(t *testing.T, s *Store, sandboxID string, scopes ...aat.Scope) (AgentToken, string) {
	t.Helper()
	row, secret, err := s.CreateAgentToken(t.Context(), AgentTokenRequest{
		SandboxID: sandboxID, OrgID: "org_1", Scopes: readSet(t, scopes...),
		Label: "test", CreatedBy: "key_1",
	})
	if err != nil {
		t.Fatalf("CreateAgentToken: %v", err)
	}
	return row, secret
}

func TestMintedTokenValidatesAndCarriesItsScopes(t *testing.T) {
	s := openTemp(t)
	row, secret := mintFor(t, s, "sb_a", aat.ScopeRead, aat.ScopeWrite)

	got, err := s.ValidateAgentToken(t.Context(), secret)
	if err != nil {
		t.Fatalf("ValidateAgentToken: %v", err)
	}
	if got.ID != row.ID || got.SandboxID != "sb_a" || got.OrgID != "org_1" {
		t.Fatalf("wrong row back: %+v", got)
	}
	if !got.Scopes.Has(aat.ScopeRead) || !got.Scopes.Has(aat.ScopeWrite) {
		t.Fatalf("scopes lost in round-trip: %v", got.Scopes.Encode())
	}
	// Granting write must not grant a shell.
	if got.Scopes.Has(aat.ScopeConsole) {
		t.Fatal("a read+write token came back holding console")
	}
}

func TestSecretIsNeverStoredOrReturnedAgain(t *testing.T) {
	s := openTemp(t)
	_, secret := mintFor(t, s, "sb_a", aat.ScopeRead)

	// The plaintext must appear in no text column of the row.
	rows, err := s.query(t.Context(), `SELECT id, sandbox_id, org_id, secret_sha256, scopes, label,
		created_by, last_used_ip, supersedes FROM agent_tokens`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		cols := make([]string, 9)
		ptrs := make([]any, 9)
		for i := range cols {
			ptrs[i] = &cols[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan: %v", err)
		}
		for i, c := range cols {
			if c != "" && strings.Contains(c, secret) {
				t.Fatalf("column %d stored the plaintext secret", i)
			}
		}
	}

	list, err := s.ListAgentTokens(t.Context(), "sb_a")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListAgentTokens: %v (%d rows)", err, len(list))
	}
}

func TestTokenBoundToOneSandbox(t *testing.T) {
	s := openTemp(t)
	_, secret := mintFor(t, s, "sb_a", aat.ScopeRead)

	got, err := s.ValidateAgentToken(t.Context(), secret)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got.SandboxID != "sb_a" {
		t.Fatalf("binding wrong: %q", got.SandboxID)
	}
	if _, err := s.GetAgentToken(t.Context(), "sb_b", got.ID); !errors.Is(err, ErrInvalidAgentToken) {
		t.Fatalf("token for sb_a was addressable as sb_b's: %v", err)
	}
}

func TestWrongSecretWithARealIDIsRejected(t *testing.T) {
	s := openTemp(t)
	row, _ := mintFor(t, s, "sb_a", aat.ScopeRead)

	// Right public id, wrong secret.
	forged := aat.Prefix + row.ID + "_" + strings.Repeat("ab", 32)
	if _, err := s.ValidateAgentToken(t.Context(), forged); !errors.Is(err, ErrInvalidAgentToken) {
		t.Fatalf("forged secret accepted: %v", err)
	}
}

func TestMalformedTokensAreRejectedWithoutATouchingTheDatabase(t *testing.T) {
	s := openTemp(t)
	for _, bad := range []string{
		"", "sf_live_abc", "sfa_", "sfa_x", "sfa__abcdefghijklmnopq",
		"sfa_zz_abcdefghijklmnopq", // non-hex id
		"sfa_abcdef_short",         // secret too short to be real
	} {
		if _, err := s.ValidateAgentToken(t.Context(), bad); !errors.Is(err, ErrInvalidAgentToken) {
			t.Fatalf("%q was not rejected: %v", bad, err)
		}
	}
}

func TestRevocationIsImmediate(t *testing.T) {
	s := openTemp(t)
	row, secret := mintFor(t, s, "sb_a", aat.ScopeRead)

	if _, err := s.ValidateAgentToken(t.Context(), secret); err != nil {
		t.Fatalf("pre-revoke validate: %v", err)
	}
	start := time.Now()
	if err := s.RevokeAgentToken(t.Context(), "sb_a", row.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := s.ValidateAgentToken(t.Context(), secret); !errors.Is(err, ErrAgentTokenRevoked) {
		t.Fatalf("revoked token still authenticates: %v", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("revocation took %v, over the one-second budget", el)
	}
}

func TestRevokeIsIdempotentAndKeepsTheFirstTimestamp(t *testing.T) {
	s := openTemp(t)
	row, _ := mintFor(t, s, "sb_a", aat.ScopeRead)

	if err := s.RevokeAgentToken(t.Context(), "sb_a", row.ID); err != nil {
		t.Fatalf("first revoke: %v", err)
	}
	first, err := s.GetAgentToken(t.Context(), "sb_a", row.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if err := s.RevokeAgentToken(t.Context(), "sb_a", row.ID); err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	again, _ := s.GetAgentToken(t.Context(), "sb_a", row.ID)
	if !again.RevokedAt.Equal(first.RevokedAt) {
		t.Fatalf("second revoke rewrote the audit timestamp: %v -> %v",
			first.RevokedAt, again.RevokedAt)
	}
}

func TestRevokingAnotherSandboxesTokenFails(t *testing.T) {
	s := openTemp(t)
	row, secret := mintFor(t, s, "sb_a", aat.ScopeRead)

	if err := s.RevokeAgentToken(t.Context(), "sb_b", row.ID); !errors.Is(err, ErrInvalidAgentToken) {
		t.Fatalf("revoked sb_a's token through sb_b: %v", err)
	}
	if _, err := s.ValidateAgentToken(t.Context(), secret); err != nil {
		t.Fatalf("cross-sandbox revoke actually took effect: %v", err)
	}
}

func TestExpiredTokenIsRejectedWithNoGrace(t *testing.T) {
	s := openTemp(t)
	row, secret := mintFor(t, s, "sb_a", aat.ScopeRead)

	if _, err := s.exec(t.Context(), `UPDATE agent_tokens SET expires_at = ? WHERE id = ?`,
		time.Now().Add(-time.Second).Unix(), row.ID); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if _, err := s.ValidateAgentToken(t.Context(), secret); !errors.Is(err, ErrAgentTokenExpired) {
		t.Fatalf("expired token accepted: %v", err)
	}
}

func TestTTLIsClampedNotUnbounded(t *testing.T) {
	s := openTemp(t)
	row, _, err := s.CreateAgentToken(t.Context(), AgentTokenRequest{
		SandboxID: "sb_a", OrgID: "org_1", Scopes: readSet(t, aat.ScopeRead),
		TTL: 3650 * 24 * time.Hour, // "ten years, please"
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if life := time.Until(row.ExpiresAt); life > maxTokenTTL+time.Minute {
		t.Fatalf("TTL not clamped: %v", life)
	}
}

func TestZeroScopesIsRefused(t *testing.T) {
	s := openTemp(t)
	if _, _, err := s.CreateAgentToken(t.Context(), AgentTokenRequest{
		SandboxID: "sb_a", OrgID: "org_1", Scopes: aat.Set{},
	}); err == nil {
		t.Fatal("minted a token that can do nothing")
	}
}

func TestRotationKeepsBothSecretsThenRetiresTheOld(t *testing.T) {
	s := openTemp(t)
	old, oldSecret := mintFor(t, s, "sb_a", aat.ScopeRead, aat.ScopeWrite)

	fresh, newSecret, err := s.RotateAgentToken(t.Context(), "sb_a", old.ID, "key_1")
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if newSecret == oldSecret || fresh.ID == old.ID {
		t.Fatal("rotation returned the same credential")
	}
	if fresh.Supersedes != old.ID {
		t.Fatalf("rotation did not record what it replaced: %q", fresh.Supersedes)
	}
	if fresh.Scopes.Encode() != old.Scopes.Encode() {
		t.Fatalf("scopes changed on rotation: %q -> %q", old.Scopes.Encode(), fresh.Scopes.Encode())
	}

	// Both work during the overlap.
	if _, err := s.ValidateAgentToken(t.Context(), oldSecret); err != nil {
		t.Fatalf("old secret died immediately: %v", err)
	}
	if _, err := s.ValidateAgentToken(t.Context(), newSecret); err != nil {
		t.Fatalf("new secret does not work: %v", err)
	}

	// Push past the overlap rather than sleeping five minutes.
	if _, err := s.exec(t.Context(), `UPDATE agent_tokens SET expires_at = ? WHERE id = ?`,
		time.Now().Add(-time.Second).Unix(), old.ID); err != nil {
		t.Fatalf("advance past overlap: %v", err)
	}
	if _, err := s.ValidateAgentToken(t.Context(), oldSecret); !errors.Is(err, ErrAgentTokenExpired) {
		t.Fatalf("old secret outlived the overlap: %v", err)
	}
	if _, err := s.ValidateAgentToken(t.Context(), newSecret); err != nil {
		t.Fatalf("new secret affected by the old one retiring: %v", err)
	}
}

func TestRotationNeverExtendsANearlyExpiredToken(t *testing.T) {
	s := openTemp(t)
	row, oldSecret := mintFor(t, s, "sb_a", aat.ScopeRead)

	// One minute left, less than the overlap; rotation must not extend it.
	soon := time.Now().Add(time.Minute).Unix()
	if _, err := s.exec(t.Context(), `UPDATE agent_tokens SET expires_at = ? WHERE id = ?`, soon, row.ID); err != nil {
		t.Fatalf("shorten: %v", err)
	}
	if _, _, err := s.RotateAgentToken(t.Context(), "sb_a", row.ID, "key_1"); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	after, err := s.GetAgentToken(t.Context(), "sb_a", row.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.ExpiresAt.Unix() > soon {
		t.Fatalf("rotation extended the old token's life: %v > %v",
			after.ExpiresAt.Unix(), soon)
	}
	if _, err := s.ValidateAgentToken(t.Context(), oldSecret); err != nil {
		t.Fatalf("old token broke before its own expiry: %v", err)
	}
}

func TestRotatingARevokedTokenIsRefused(t *testing.T) {
	s := openTemp(t)
	row, _ := mintFor(t, s, "sb_a", aat.ScopeRead)
	if err := s.RevokeAgentToken(t.Context(), "sb_a", row.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, _, err := s.RotateAgentToken(t.Context(), "sb_a", row.ID, "key_1"); !errors.Is(err, ErrAgentTokenRevoked) {
		t.Fatalf("rotated a revoked token: %v", err)
	}
}

func TestConsoleTokensRevokedOnPauseAndOthersSurvive(t *testing.T) {
	s := openTemp(t)
	console, consoleSecret := mintFor(t, s, "sb_a", aat.ScopeRead, aat.ScopeConsole)
	watch, watchSecret := mintFor(t, s, "sb_a", aat.ScopeRead)
	other, otherSecret := mintFor(t, s, "sb_b", aat.ScopeConsole)

	ids, err := s.RevokeAgentTokensWithScope(t.Context(), "sb_a", aat.ScopeConsole)
	if err != nil {
		t.Fatalf("revoke by scope: %v", err)
	}
	if len(ids) != 1 || ids[0] != console.ID {
		t.Fatalf("wrong tokens revoked: %v (wanted just %s)", ids, console.ID)
	}
	if _, err := s.ValidateAgentToken(t.Context(), consoleSecret); !errors.Is(err, ErrAgentTokenRevoked) {
		t.Fatalf("console token survived the pause: %v", err)
	}
	// The read-only share link must keep working.
	if _, err := s.ValidateAgentToken(t.Context(), watchSecret); err != nil {
		t.Fatalf("read token revoked by a console-only sweep: %v (%s)", err, watch.ID)
	}
	if _, err := s.ValidateAgentToken(t.Context(), otherSecret); err != nil {
		t.Fatalf("sb_b's token revoked by sb_a's pause: %v (%s)", err, other.ID)
	}
}

func TestAllTokensRevokedForASandbox(t *testing.T) {
	s := openTemp(t)
	_, a := mintFor(t, s, "sb_a", aat.ScopeRead)
	_, b := mintFor(t, s, "sb_a", aat.ScopeWrite, aat.ScopeConsole)
	_, keep := mintFor(t, s, "sb_b", aat.ScopeRead)

	n, err := s.RevokeAgentTokensForSandbox(t.Context(), "sb_a")
	if err != nil {
		t.Fatalf("revoke all: %v", err)
	}
	if n != 2 {
		t.Fatalf("revoked %d tokens, wanted 2", n)
	}
	for _, secret := range []string{a, b} {
		if _, err := s.ValidateAgentToken(t.Context(), secret); !errors.Is(err, ErrAgentTokenRevoked) {
			t.Fatalf("token survived destroy: %v", err)
		}
	}
	if _, err := s.ValidateAgentToken(t.Context(), keep); err != nil {
		t.Fatalf("another sandbox's token was revoked: %v", err)
	}
}

func TestListKeepsRevokedTokensForAudit(t *testing.T) {
	s := openTemp(t)
	row, _ := mintFor(t, s, "sb_a", aat.ScopeRead)
	if err := s.RevokeAgentToken(t.Context(), "sb_a", row.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	list, err := s.ListAgentTokens(t.Context(), "sb_a")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || !list[0].Revoked() || list[0].CreatedBy != "key_1" {
		t.Fatalf("audit history lost: %+v", list)
	}
	if list[0].Active(time.Now()) {
		t.Fatal("a revoked token reports itself active")
	}
}

func TestUnknownScopeInAStoredRowDoesNotBreakTheToken(t *testing.T) {
	s := openTemp(t)
	row, secret := mintFor(t, s, "sb_a", aat.ScopeRead)
	// Simulates a rollback: a newer binary wrote a scope this one does not know.
	if _, err := s.exec(t.Context(), `UPDATE agent_tokens SET scopes = ? WHERE id = ?`,
		"agent:read agent:teleport", row.ID); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := s.ValidateAgentToken(t.Context(), secret)
	if err != nil {
		t.Fatalf("an unknown scope invalidated the whole token: %v", err)
	}
	// The known scope survives; the unknown one is simply not granted.
	if !got.Scopes.Has(aat.ScopeRead) || len(got.Scopes) != 1 {
		t.Fatalf("scopes mishandled: %q", got.Scopes.Encode())
	}
}

func TestTouchIsSampledNotWrittenEveryRequest(t *testing.T) {
	s := openTemp(t)
	row, secret := mintFor(t, s, "sb_a", aat.ScopeRead)

	s.TouchAgentToken(t.Context(), row, "203.0.113.9")
	// The write is async; poll.
	var seen AgentToken
	for range 100 {
		seen, _ = s.GetAgentToken(t.Context(), "sb_a", row.ID)
		if !seen.LastUsedAt.IsZero() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if seen.LastUsedAt.IsZero() {
		t.Fatal("first use was never recorded")
	}
	if seen.LastUsedIP != "203.0.113.9" {
		t.Fatalf("source address not attributed: %q", seen.LastUsedIP)
	}

	// A second use inside the sampling window must not write again.
	fresh, err := s.ValidateAgentToken(t.Context(), secret)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	s.TouchAgentToken(t.Context(), fresh, "198.51.100.1")
	time.Sleep(20 * time.Millisecond)
	after, _ := s.GetAgentToken(t.Context(), "sb_a", row.ID)
	if after.LastUsedIP != "203.0.113.9" {
		t.Fatalf("sampling window ignored; IP was rewritten to %q", after.LastUsedIP)
	}
}

func TestPurgeRemovesOnlyDeadTokens(t *testing.T) {
	s := openTemp(t)
	live, liveSecret := mintFor(t, s, "sb_a", aat.ScopeRead)
	dead, _ := mintFor(t, s, "sb_a", aat.ScopeRead)
	if _, err := s.exec(t.Context(), `UPDATE agent_tokens SET expires_at = ? WHERE id = ?`,
		time.Now().Add(-48*time.Hour).Unix(), dead.ID); err != nil {
		t.Fatalf("expire: %v", err)
	}

	n, err := s.PurgeExpiredAgentTokens(t.Context(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Fatalf("purged %d rows, wanted 1", n)
	}
	if _, err := s.ValidateAgentToken(t.Context(), liveSecret); err != nil {
		t.Fatalf("purge took a live token: %v (%s)", err, live.ID)
	}
}

func TestConsoleTokensExpireInMinutesNotDays(t *testing.T) {
	st := openTemp(t)

	for name, scopes := range map[string][]aat.Scope{
		"console alone":        {aat.ScopeConsole},
		"console among others": {aat.ScopeRead, aat.ScopeWrite, aat.ScopeConsole},
	} {
		set, err := aat.NewSet(scopes)
		if err != nil {
			t.Fatal(err)
		}
		tok, _, err := st.CreateAgentToken(t.Context(), AgentTokenRequest{
			SandboxID: "sb1", OrgID: "org1", Scopes: set,
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if d := time.Until(tok.ExpiresAt); d > 20*time.Minute {
			t.Errorf("%s: default lifetime %v — a shell must not live that long", name, d)
		}

		// Asking for longer is clamped: the console TTL is a ceiling, not a default.
		tok, _, err = st.CreateAgentToken(t.Context(), AgentTokenRequest{
			SandboxID: "sb1", OrgID: "org1", Scopes: set, TTL: 30 * 24 * time.Hour,
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if d := time.Until(tok.ExpiresAt); d > 20*time.Minute {
			t.Errorf("%s: a caller obtained a %v shell by asking for one", name, d)
		}
	}
}

func TestNonConsoleTokensKeepTheirLongerBounds(t *testing.T) {
	st := openTemp(t)

	write, _ := aat.NewSet([]aat.Scope{aat.ScopeRead, aat.ScopeWrite})
	tok, _, err := st.CreateAgentToken(t.Context(), AgentTokenRequest{
		SandboxID: "sb1", OrgID: "org1", Scopes: write, TTL: 20 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(tok.ExpiresAt); d < 19*24*time.Hour {
		t.Fatalf("a 20-day integration token was cut to %v", d)
	}

	// A read-only token (the share-link shape) defaults to a day.
	readOnly, _ := aat.NewSet([]aat.Scope{aat.ScopeRead})
	tok, _, err = st.CreateAgentToken(t.Context(), AgentTokenRequest{
		SandboxID: "sb1", OrgID: "org1", Scopes: readOnly,
	})
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(tok.ExpiresAt); d > 26*time.Hour || d < 22*time.Hour {
		t.Errorf("read-only default lifetime = %v, want about 24h", d)
	}

	// The general 30-day ceiling still applies to non-console tokens.
	tok, _, err = st.CreateAgentToken(t.Context(), AgentTokenRequest{
		SandboxID: "sb1", OrgID: "org1", Scopes: write, TTL: 365 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(tok.ExpiresAt); d > 31*24*time.Hour {
		t.Errorf("a year-long token was granted: %v", d)
	}
}
