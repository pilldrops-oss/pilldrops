package cron

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// fakeDomains is an in-memory DomainStore; every row is due every sweep.
type fakeDomains struct {
	rows map[string]store.Domain
}

func (f *fakeDomains) DomainsDue(_ context.Context, _ int64, _ int) ([]store.Domain, error) {
	out := make([]store.Domain, 0, len(f.rows))
	for _, d := range f.rows {
		out = append(out, d)
	}
	return out, nil
}

func (f *fakeDomains) SetDomainStatus(_ context.Context, host, status, errText string, checkedAt int64) error {
	d := f.rows[host]
	d.Status, d.Error, d.CheckedAt = status, errText, checkedAt
	f.rows[host] = d
	return nil
}

func (f *fakeDomains) SetDomainLegacySeen(_ context.Context, host string, at int64) error {
	d := f.rows[host]
	d.LegacyTargetSeenAt = at
	f.rows[host] = d
	return nil
}

func (f *fakeDomains) GetDomain(_ context.Context, host string) (store.Domain, bool, error) {
	d, ok := f.rows[host]
	return d, ok, nil
}

func (f *fakeDomains) SetDomainTXT(_ context.Context, host, token string, requiredAt int64) error {
	d := f.rows[host]
	if d.TXTToken == "" {
		d.TXTToken = token
	}
	d.TXTRequiredAt = requiredAt
	f.rows[host] = d
	return nil
}

// verifierOver builds a verifier answering from fixed DNS tables.
func verifierOver(f *fakeDomains, cnames map[string]string, hosts map[string][]string) *DomainVerifier {
	v := NewDomainVerifier(f, func(d store.Domain) string {
		if d.Wildcard {
			return "pilldrops.com"
		}
		return strconv.Itoa(d.Port) + "-" + d.SandboxID + ".pilldrops.com"
	})
	v.LookupCNAME = func(_ context.Context, host string) (string, error) {
		if c, ok := cnames[host]; ok {
			return c, nil
		}
		if _, ok := hosts[host]; ok {
			return host, nil // no CNAME: resolvers answer with the name itself
		}
		return "", errors.New("lookup " + host + ": no such host")
	}
	v.LookupHost = func(_ context.Context, host string) ([]string, error) {
		// A real resolver follows the CNAME and answers with the target's
		// addresses; the target comes back with the root dot.
		if c, ok := cnames[host]; ok {
			host = strings.TrimSuffix(c, ".")
		}
		if a, ok := hosts[host]; ok {
			return a, nil
		}
		return nil, errors.New("lookup " + host + ": no such host")
	}
	v.LookupTXT = func(_ context.Context, host string) ([]string, error) {
		return nil, errors.New("lookup " + host + ": no such host")
	}
	return v
}

// withTXT answers TXT lookups from a fixed table and records what was asked.
func withTXT(v *DomainVerifier, txts map[string][]string) *[]string {
	var asked []string
	v.LookupTXT = func(_ context.Context, host string) ([]string, error) {
		asked = append(asked, host)
		if t, ok := txts[host]; ok {
			return t, nil
		}
		return nil, errors.New("lookup " + host + ": no such host")
	}
	return &asked
}

func TestVerifierActivatesAMatchingCNAME(t *testing.T) {
	f := &fakeDomains{rows: map[string]store.Domain{
		"app.example.com": {Host: "app.example.com", SandboxID: "sb1", Port: 8000, Status: "pending"},
	}}
	v := verifierOver(f,
		map[string]string{"app.example.com": "8000-sb1.pilldrops.com."},
		map[string][]string{"8000-sb1.pilldrops.com": {"192.0.2.9"}})

	v.Sweep(t.Context())

	got := f.rows["app.example.com"]
	if got.Status != "active" || got.Error != "" {
		t.Fatalf("row = %+v, want active with no error", got)
	}
}

// flatHosts is DNS where flat.example.com shares the target's address, as an
// A record, a flattened apex, or any other zone behind the same proxy does.
var flatHosts = map[string][]string{
	"flat.example.com":       {"192.0.2.9"},
	"8000-sb1.pilldrops.com": {"192.0.2.9"},
	"other.pilldrops.com":    {"198.51.100.1"},
}

// The A-record form and CNAME flattening both land on the address comparison,
// which a shared proxy address satisfies for anyone, so it also needs the TXT.
func TestVerifierActivatesOnMatchingAddressesWithTheTXTRecord(t *testing.T) {
	f := &fakeDomains{rows: map[string]store.Domain{
		"flat.example.com": {Host: "flat.example.com", SandboxID: "sb1", Port: 8000, Status: "pending", TXTToken: "tok1", TXTRequiredAt: 5},
	}}
	v := verifierOver(f, nil, flatHosts)
	withTXT(v, map[string][]string{"_pilldrops-challenge.flat.example.com": {"unrelated", "tok1"}})

	v.Sweep(t.Context())

	if got := f.rows["flat.example.com"]; got.Status != "active" || got.TXTRequiredAt != 0 {
		t.Fatalf("row = %+v, want active with no TXT deadline", got)
	}
}

// A row created after the TXT proof shipped gets no grace: a new row, or one
// that was active by CNAME and moved to an A record, waits for the TXT.
func TestVerifierWantsTheTXTRecordForAnAddressMatch(t *testing.T) {
	now := time.Unix(1_000_000_000, 0)
	for _, status := range []string{"pending", "active"} {
		f := &fakeDomains{rows: map[string]store.Domain{
			"flat.example.com": {Host: "flat.example.com", SandboxID: "sb1", Port: 8000, Status: status, TXTToken: "tok1"},
		}}
		v := verifierOver(f, nil, flatHosts)
		v.Now = func() time.Time { return now }
		v.Sweep(t.Context())

		got := f.rows["flat.example.com"]
		if got.Status != "pending" || got.TXTRequiredAt != now.Unix() {
			t.Fatalf("was %s: row = %+v, want pending, the TXT required from now", status, got)
		}
		if !strings.Contains(got.Error, `"_pilldrops-challenge.flat.example.com TXT tok1"`) {
			t.Errorf("was %s: error %q does not name the record to create", status, got.Error)
		}
	}
}

func TestVerifierRefusesAWrongTXTToken(t *testing.T) {
	f := &fakeDomains{rows: map[string]store.Domain{
		"flat.example.com": {Host: "flat.example.com", SandboxID: "sb1", Port: 8000, Status: "pending", TXTToken: "tok1"},
	}}
	v := verifierOver(f, nil, flatHosts)
	withTXT(v, map[string][]string{"_pilldrops-challenge.flat.example.com": {"tok2"}})

	v.Sweep(t.Context())

	if got := f.rows["flat.example.com"]; got.Status != "pending" || got.TXTToken != "tok1" {
		t.Fatalf("row = %+v, want pending with its token unchanged", got)
	}
}

// Only the DNS owner can create the exact CNAME, so it needs no TXT, and it
// ends any TXT clock an earlier address match started. It still gets a token,
// so a later move to an address match is a new requirement, not a grace.
func TestVerifierNeedsNoTXTForTheCNAME(t *testing.T) {
	f := &fakeDomains{rows: map[string]store.Domain{
		"app.example.com": {Host: "app.example.com", SandboxID: "sb1", Port: 8000, Status: "pending", TXTRequiredAt: 5},
	}}
	v := verifierOver(f,
		map[string]string{"app.example.com": "8000-sb1.pilldrops.com."},
		map[string][]string{"8000-sb1.pilldrops.com": {"192.0.2.9"}})
	asked := withTXT(v, nil)

	v.Sweep(t.Context())

	got := f.rows["app.example.com"]
	if got.Status != "active" || got.TXTRequiredAt != 0 || len(got.TXTToken) < 26 {
		t.Fatalf("row = %+v, want active, no TXT deadline and a token minted", got)
	}
	if len(*asked) != 0 {
		t.Errorf("looked up TXT %q for a CNAME row", *asked)
	}
}

// A row that was active by address before the TXT proof shipped has no token.
// It gets one, once, and keeps routing for TXTGrace while it shows the record.
func TestVerifierGivesAnExistingAddressRowItsGrace(t *testing.T) {
	start := time.Unix(1_000_000_000, 0)
	now := start
	f := &fakeDomains{rows: map[string]store.Domain{
		"flat.example.com": {Host: "flat.example.com", SandboxID: "sb1", Port: 8000, Status: "active"},
	}}
	v := verifierOver(f, nil, flatHosts)
	v.Now = func() time.Time { return now }

	v.Sweep(t.Context())
	got := f.rows["flat.example.com"]
	token := got.TXTToken
	if got.Status != "active" || got.TXTRequiredAt != start.Add(TXTGrace).Unix() {
		t.Fatalf("first ask: row = %+v, want active with the TXT required %s from now", got, TXTGrace)
	}
	if len(token) < 26 || token != strings.ToLower(token) {
		t.Fatalf("token %q, want at least 128 random bits in lower case", token)
	}

	now = start.Add(TXTGrace - time.Hour)
	v.Sweep(t.Context())
	if got := f.rows["flat.example.com"]; got.Status != "active" || got.TXTToken != token {
		t.Fatalf("inside the grace: row = %+v, want active with token %q kept", got, token)
	}

	now = start.Add(TXTGrace + time.Second)
	v.Sweep(t.Context())
	got = f.rows["flat.example.com"]
	if got.Status != "pending" || got.TXTToken != token || got.TXTRequiredAt != start.Add(TXTGrace).Unix() {
		t.Fatalf("after the grace: row = %+v, want pending with token and deadline kept", got)
	}
	if !strings.Contains(got.Error, `"_pilldrops-challenge.flat.example.com TXT `+token+`"`) {
		t.Errorf("error %q does not name the record to create", got.Error)
	}
}

// A row seen once by CNAME after the deploy is not a pre-deploy address row:
// if it later matches only by address, say after the domain changed hands, it
// waits for the TXT at once.
func TestVerifierGivesNoGraceToARowFirstSeenByCNAME(t *testing.T) {
	now := time.Unix(1_000_000_000, 0)
	f := &fakeDomains{rows: map[string]store.Domain{
		"flat.example.com": {Host: "flat.example.com", SandboxID: "sb1", Port: 8000, Status: "active"},
	}}
	cname := verifierOver(f, map[string]string{"flat.example.com": "8000-sb1.pilldrops.com."}, flatHosts)
	cname.Now = func() time.Time { return now }
	cname.Sweep(t.Context())

	now = now.Add(90 * 24 * time.Hour)
	addr := verifierOver(f, nil, flatHosts)
	addr.Now = func() time.Time { return now }
	addr.Sweep(t.Context())

	if got := f.rows["flat.example.com"]; got.Status != "pending" || got.TXTRequiredAt != now.Unix() {
		t.Fatalf("row = %+v, want pending with the TXT required from now", got)
	}
}

// Replicas racing on a token-less row mint different tokens; the store keeps
// the first, and the error must name that one.
func TestVerifierNamesTheStoredTokenAfterARace(t *testing.T) {
	f := &racingDomains{fakeDomains{rows: map[string]store.Domain{
		"flat.example.com": {Host: "flat.example.com", SandboxID: "sb1", Port: 8000, Status: "pending"},
	}}}
	v := verifierOver(&f.fakeDomains, nil, flatHosts)
	v.st = f
	v.Sweep(t.Context())

	if got := f.rows["flat.example.com"]; got.TXTToken != "winner" || !strings.Contains(got.Error, "TXT winner") {
		t.Fatalf("row = %+v, want the stored token named", got)
	}
}

// racingDomains is a store where another replica wrote its token first.
type racingDomains struct{ fakeDomains }

func (r *racingDomains) SetDomainTXT(ctx context.Context, host, _ string, requiredAt int64) error {
	return r.fakeDomains.SetDomainTXT(ctx, host, "winner", requiredAt)
}

// DNS consoles differ on whether the quotes are part of the value.
func TestVerifierToleratesQuotesAndSpacesAroundTheTXTValue(t *testing.T) {
	f := &fakeDomains{rows: map[string]store.Domain{
		"flat.example.com": {Host: "flat.example.com", SandboxID: "sb1", Port: 8000, Status: "pending", TXTToken: "tok1"},
	}}
	v := verifierOver(f, nil, flatHosts)
	withTXT(v, map[string][]string{"_pilldrops-challenge.flat.example.com": {` "tok1" `}})

	v.Sweep(t.Context())

	if got := f.rows["flat.example.com"]; got.Status != "active" {
		t.Fatalf("row = %+v, want active", got)
	}
}

func TestVerifierEndsTheGraceOnceTheTXTExists(t *testing.T) {
	now := time.Unix(1_000_000_000, 0)
	f := &fakeDomains{rows: map[string]store.Domain{
		"flat.example.com": {Host: "flat.example.com", SandboxID: "sb1", Port: 8000, Status: "active",
			TXTToken: "tok1", TXTRequiredAt: now.Add(time.Hour).Unix()},
	}}
	v := verifierOver(f, nil, flatHosts)
	v.Now = func() time.Time { return now }
	withTXT(v, map[string][]string{"_pilldrops-challenge.flat.example.com": {"tok1"}})

	v.Sweep(t.Context())

	if got := f.rows["flat.example.com"]; got.Status != "active" || got.TXTRequiredAt != 0 {
		t.Fatalf("row = %+v, want active with the deadline cleared", got)
	}
}

// A wildcard has no single name to hold the TXT, so it sits at the base domain.
func TestVerifierReadsAWildcardTXTAtItsBaseDomain(t *testing.T) {
	f := &fakeDomains{rows: map[string]store.Domain{
		"*.vms.example.com": {Host: "*.vms.example.com", Wildcard: true, Status: "pending", TXTToken: "tok1"},
	}}
	v := verifierOver(f, nil, map[string][]string{"pilldrops.com": {"192.0.2.9"}})
	inner := v.LookupHost
	v.LookupHost = func(ctx context.Context, host string) ([]string, error) {
		if strings.HasSuffix(host, ".vms.example.com") {
			return []string{"192.0.2.9"}, nil
		}
		return inner(ctx, host)
	}
	asked := withTXT(v, map[string][]string{"_pilldrops-challenge.vms.example.com": {"tok1"}})

	v.Sweep(t.Context())

	if got := f.rows["*.vms.example.com"]; got.Status != "active" || len(*asked) != 1 || (*asked)[0] != "_pilldrops-challenge.vms.example.com" {
		t.Fatalf("row = %+v with TXT asked at %q, want active from the base domain's record", got, *asked)
	}
}

func TestVerifierExplainsAWrongRecord(t *testing.T) {
	f := &fakeDomains{rows: map[string]store.Domain{
		"wrong.example.com": {Host: "wrong.example.com", SandboxID: "sb1", Port: 8000, Status: "pending"},
		"gone.example.com":  {Host: "gone.example.com", SandboxID: "sb1", Port: 8000, Status: "pending"},
	}}
	v := verifierOver(f,
		map[string]string{"wrong.example.com": "example.com."},
		map[string][]string{
			"example.com":            {"198.51.100.7"},
			"8000-sb1.pilldrops.com": {"192.0.2.9"},
		})

	v.Sweep(t.Context())

	wrong := f.rows["wrong.example.com"]
	if wrong.Status != "pending" {
		t.Fatalf("wrong record became %q", wrong.Status)
	}
	if !strings.Contains(wrong.Error, "example.com") || !strings.Contains(wrong.Error, "8000-sb1.pilldrops.com") {
		t.Errorf("error %q does not say what was found and what is wanted", wrong.Error)
	}
	if !strings.Contains(wrong.Error, "TTL") {
		t.Errorf("error %q does not mention the caching delay", wrong.Error)
	}

	gone := f.rows["gone.example.com"]
	if gone.Status != "pending" || !strings.Contains(gone.Error, "does not resolve") {
		t.Errorf("unresolvable host = %+v", gone)
	}
}

// An active host whose record was removed stops routing on the next sweep.
func TestVerifierDemotesAHostThatStoppedResolving(t *testing.T) {
	f := &fakeDomains{rows: map[string]store.Domain{
		"app.example.com": {Host: "app.example.com", SandboxID: "sb1", Port: 8000, Status: "active"},
	}}
	v := verifierOver(f, nil, map[string][]string{"8000-sb1.pilldrops.com": {"192.0.2.9"}})

	v.Sweep(t.Context())

	if got := f.rows["app.example.com"]; got.Status != "pending" || got.Error == "" {
		t.Fatalf("row = %+v, want pending with an error", got)
	}
}

// A wildcard is proved by a label under it, never by the bare name.
func TestVerifierResolvesALabelUnderAWildcard(t *testing.T) {
	f := &fakeDomains{rows: map[string]store.Domain{
		"*.vms.example.com": {Host: "*.vms.example.com", Wildcard: true, Status: "pending"},
	}}
	var asked []string
	v := verifierOver(f, nil, map[string][]string{"pilldrops.com": {"192.0.2.9"}})
	inner := v.LookupCNAME
	v.LookupCNAME = func(ctx context.Context, host string) (string, error) {
		asked = append(asked, host)
		if strings.HasSuffix(host, ".vms.example.com") {
			return "pilldrops.com.", nil
		}
		return inner(ctx, host)
	}

	v.Sweep(t.Context())

	if got := f.rows["*.vms.example.com"]; got.Status != "active" {
		t.Fatalf("row = %+v, want active", got)
	}
	if len(asked) == 0 || strings.HasPrefix(asked[0], "*") || !strings.HasSuffix(asked[0], ".vms.example.com") {
		t.Errorf("resolved %q, want a label under the wildcard", asked)
	}
}

// movedVerifier is verifierOver after the sandbox domain moved from
// pilldrops.com to pilldrops.app, at a fixed now.
func movedVerifier(f *fakeDomains, cnames map[string]string, hosts map[string][]string, now time.Time) *DomainVerifier {
	v := verifierOver(f, cnames, hosts)
	v.Target = func(d store.Domain) string {
		if d.Wildcard {
			return "pilldrops.app"
		}
		return strconv.Itoa(d.Port) + "-" + d.SandboxID + ".pilldrops.app"
	}
	v.LegacyTargets = func(d store.Domain) []string {
		if d.Wildcard {
			return []string{"pilldrops.com"}
		}
		return []string{strconv.Itoa(d.Port) + "-" + d.SandboxID + ".pilldrops.com"}
	}
	v.Now = func() time.Time { return now }
	return v
}

func TestVerifierAcceptsALegacyCNAMEAndStartsItsClock(t *testing.T) {
	now := time.Unix(1_000_000_000, 0)
	f := &fakeDomains{rows: map[string]store.Domain{
		"app.example.com": {Host: "app.example.com", SandboxID: "sb1", Port: 8000, Status: "active"},
	}}
	movedVerifier(f,
		map[string]string{"app.example.com": "8000-sb1.pilldrops.com."},
		map[string][]string{"8000-sb1.pilldrops.com": {"192.0.2.9"}}, now).Sweep(t.Context())

	got := f.rows["app.example.com"]
	if got.Status != "active" || got.LegacyTargetSeenAt != now.Unix() {
		t.Fatalf("row = %+v, want active with the legacy clock started now", got)
	}
}

func TestVerifierLapsesALegacyCNAMEAfterNinetyDaysWithoutTraffic(t *testing.T) {
	now := time.Unix(1_000_000_000, 0)
	for _, tc := range []struct {
		idle   time.Duration
		status string
	}{
		{89 * 24 * time.Hour, "active"},
		{91 * 24 * time.Hour, "pending"},
	} {
		seen := now.Add(-tc.idle).Unix()
		f := &fakeDomains{rows: map[string]store.Domain{
			"app.example.com": {Host: "app.example.com", SandboxID: "sb1", Port: 8000, Status: "active", LegacyTargetSeenAt: seen},
		}}
		movedVerifier(f,
			map[string]string{"app.example.com": "8000-sb1.pilldrops.com."},
			map[string][]string{"8000-sb1.pilldrops.com": {"192.0.2.9"}}, now).Sweep(t.Context())

		got := f.rows["app.example.com"]
		if got.Status != tc.status || got.LegacyTargetSeenAt != seen {
			t.Errorf("idle %s: row = %+v, want %s with the marker kept", tc.idle, got, tc.status)
		}
		if tc.status == "pending" && !strings.Contains(got.Error, "app.example.com CNAME 8000-sb1.pilldrops.app") {
			t.Errorf("idle %s: error %q does not name the record to create", tc.idle, got.Error)
		}
	}
}

func TestVerifierClearsTheMarkerOnceDNSMoves(t *testing.T) {
	now := time.Unix(1_000_000_000, 0)
	f := &fakeDomains{rows: map[string]store.Domain{
		"*.vms.example.com": {Host: "*.vms.example.com", Wildcard: true, Status: "active", LegacyTargetSeenAt: 5},
	}}
	v := movedVerifier(f, nil, map[string][]string{"pilldrops.app": {"192.0.2.9"}}, now)
	inner := v.LookupHost
	v.LookupHost = func(ctx context.Context, host string) ([]string, error) {
		if strings.HasSuffix(host, ".vms.example.com") {
			return []string{"192.0.2.9"}, nil
		}
		return inner(ctx, host)
	}
	v.Sweep(t.Context())

	got := f.rows["*.vms.example.com"]
	if got.Status != "active" || got.LegacyTargetSeenAt != 0 {
		t.Fatalf("row = %+v, want active and no longer marked legacy", got)
	}
}

// The old domain's addresses are a shared proxy's, so matching them proves
// nothing: only its exact CNAME may keep a row alive.
func TestVerifierRefusesALegacyAddressMatchWithoutItsCNAME(t *testing.T) {
	now := time.Unix(1_000_000_000, 0)
	f := &fakeDomains{rows: map[string]store.Domain{
		"shop.other.com": {Host: "shop.other.com", SandboxID: "sb1", Port: 8000, Status: "pending"},
	}}
	movedVerifier(f,
		map[string]string{"shop.other.com": "edge.other.net."},
		map[string][]string{
			"8000-sb1.pilldrops.com": {"104.21.16.5"},
			"8000-sb1.pilldrops.app": {"192.0.2.9"},
			"shop.other.com":         {"104.21.16.5"},
		}, now).Sweep(t.Context())

	got := f.rows["shop.other.com"]
	if got.Status != "pending" || got.LegacyTargetSeenAt != 0 {
		t.Fatalf("row = %+v, want pending and unmarked", got)
	}
}
