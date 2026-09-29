package cron

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

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
	return v
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

// The A-record form and CNAME flattening both land on the address comparison.
func TestVerifierActivatesOnMatchingAddresses(t *testing.T) {
	f := &fakeDomains{rows: map[string]store.Domain{
		"flat.example.com": {Host: "flat.example.com", SandboxID: "sb1", Port: 8000, Status: "pending"},
	}}
	v := verifierOver(f, nil, map[string][]string{
		"flat.example.com":       {"192.0.2.9"},
		"8000-sb1.pilldrops.com": {"192.0.2.9"},
		"other.pilldrops.com":    {"198.51.100.1"},
	})

	v.Sweep(t.Context())

	if got := f.rows["flat.example.com"]; got.Status != "active" {
		t.Fatalf("row = %+v, want active", got)
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
