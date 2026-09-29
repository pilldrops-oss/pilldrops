package cron

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// The domain verifier is the only thing that turns a customer hostname from
// 'pending' into 'active'. Routing and certificate issuance both read that
// status, so a host nobody pointed at us can never be served or get a
// certificate, however it was added.

// DomainStore is the persistence the verifier needs.
type DomainStore interface {
	DomainsDue(ctx context.Context, staleBefore int64, limit int) ([]store.Domain, error)
	SetDomainStatus(ctx context.Context, host, status, errText string, checkedAt int64) error
}

const (
	// DomainCheckInterval is how often pending rows are resolved.
	DomainCheckInterval = 30 * time.Second
	// DomainRecheckAfter is how long an active row keeps routing without
	// proof; after it, a record that has gone away demotes the host.
	DomainRecheckAfter = 10 * time.Minute
	// domainsPerSweep bounds one tick's DNS work.
	domainsPerSweep = 200
	// dnsTimeout bounds one lookup; a slow resolver must not stall the sweep.
	dnsTimeout = 5 * time.Second
)

// dnsHint is appended to every failure: the most common support question is
// "I fixed the record, why is it still pending".
const dnsHint = " (resolvers cache NXDOMAIN, allow up to the record's TTL)"

// DomainVerifier resolves customer hostnames and records what it found.
type DomainVerifier struct {
	st DomainStore
	// Target is the CNAME the customer was told to create for a row. Supplied
	// by the control plane, which owns the platform host format.
	Target func(store.Domain) string
	// LookupCNAME and LookupHost default to the process resolver. They are
	// fields, not a *net.Resolver, so a test can answer without a DNS server
	// and an operator can still point them at a custom net.Resolver's methods.
	LookupCNAME func(ctx context.Context, host string) (string, error)
	LookupHost  func(ctx context.Context, host string) ([]string, error)
	Now         func() time.Time
	Log         *slog.Logger
}

func NewDomainVerifier(st DomainStore, target func(store.Domain) string) *DomainVerifier {
	return &DomainVerifier{st: st, Target: target}
}

func (v *DomainVerifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

func (v *DomainVerifier) log() *slog.Logger {
	if v.Log != nil {
		return v.Log
	}
	return slog.Default()
}

func (v *DomainVerifier) lookupCNAME(ctx context.Context, host string) (string, error) {
	if v.LookupCNAME != nil {
		return v.LookupCNAME(ctx, host)
	}
	return net.DefaultResolver.LookupCNAME(ctx, host)
}

func (v *DomainVerifier) lookupHost(ctx context.Context, host string) ([]string, error) {
	if v.LookupHost != nil {
		return v.LookupHost(ctx, host)
	}
	return net.DefaultResolver.LookupHost(ctx, host)
}

// Run verifies due rows every DomainCheckInterval until ctx is cancelled.
// Idempotent, so every replica may run it: two replicas writing the same
// status is the same row either way.
func (v *DomainVerifier) Run(ctx context.Context) {
	t := time.NewTicker(DomainCheckInterval)
	defer t.Stop()
	for {
		v.Sweep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sweep verifies every due row once and returns how many it checked.
func (v *DomainVerifier) Sweep(ctx context.Context) int {
	stale := v.now().Add(-DomainRecheckAfter).Unix()
	rows, err := v.st.DomainsDue(ctx, stale, domainsPerSweep)
	if err != nil {
		v.log().WarnContext(ctx, "could not list domains to verify",
			"component", "domains", "err", err)
		return 0
	}
	for _, d := range rows {
		if ctx.Err() != nil {
			return 0
		}
		status, detail := "pending", ""
		if err := v.check(ctx, d); err == nil {
			status = "active"
		} else {
			detail = err.Error()
		}
		if status != d.Status {
			v.log().InfoContext(ctx, "custom domain changed status",
				"component", "domains", "host", d.Host, "from", d.Status, "to", status, "detail", detail)
		}
		if err := v.st.SetDomainStatus(ctx, d.Host, status, detail, v.now().Unix()); err != nil {
			v.log().WarnContext(ctx, "could not record a domain verification",
				"component", "domains", "host", d.Host, "err", err)
		}
	}
	return len(rows)
}

// check resolves one row. nil means the customer's record points at us: either
// the CNAME is the exact target we handed out, or the name resolves to the same
// addresses that target does (CNAME flattening at the DNS provider, and the
// A-record form, both land here). Anything else is the text the customer sees.
func (v *DomainVerifier) check(ctx context.Context, d store.Domain) error {
	target := v.Target(d)
	if target == "" {
		return fmt.Errorf("this deployment has no sandbox domain configured, so there is no record to verify")
	}
	name := d.Host
	if d.Wildcard {
		// A random label under the wildcard: a fixed one would be answered from
		// the resolver's cache long after the customer fixed the record.
		// crypto/rand, so the label a deployment is about to ask for is not
		// predictable from the ones it asked for before.
		name = "pdverify" + strings.ToLower(rand.Text()[:10]) + "." + strings.TrimPrefix(d.Host, "*.")
	}
	ctx, cancel := context.WithTimeout(ctx, dnsTimeout)
	defer cancel()

	cname, cnameErr := v.lookupCNAME(ctx, name)
	if cnameErr == nil && sameHost(cname, target) {
		return nil
	}
	want, wantErr := v.lookupHost(ctx, target)
	got, gotErr := v.lookupHost(ctx, name)
	if gotErr != nil {
		return fmt.Errorf("%s does not resolve: %w%s", name, gotErr, dnsHint)
	}
	if wantErr == nil {
		for _, a := range got {
			if slices.Contains(want, a) {
				return nil
			}
		}
	}
	// Name what the customer's DNS actually says: the CNAME when there is one,
	// the addresses when the name is an A record (a resolver answers a
	// CNAME-less name with itself, which would be a useless thing to report).
	found := strings.TrimSuffix(cname, ".")
	if cnameErr != nil || found == "" || sameHost(found, name) {
		found = strings.Join(got, ", ")
	}
	return fmt.Errorf("%s points at %s, not at %s: create the record %q%s",
		name, found, target, name+" CNAME "+target, dnsHint)
}

// sameHost compares two DNS names ignoring case and the root dot.
func sameHost(a, b string) bool {
	return strings.EqualFold(strings.TrimSuffix(a, "."), strings.TrimSuffix(b, "."))
}
