package store

import (
	"errors"
	"testing"
)

func TestDomainRoundTripAndSandboxDestroy(t *testing.T) {
	st := openTemp(t)
	ctx := t.Context()

	if err := st.SaveSandbox(ctx, SandboxRecord{
		ID: "sb-1", Template: "base", OrgID: "org1", State: "running", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("save sandbox: %v", err)
	}
	if err := st.PutDomain(ctx, Domain{
		Host: "App.Example.Com", OrgID: "org1", SandboxID: "sb-1", Port: 8000, CreatedAt: 10,
	}); err != nil {
		t.Fatalf("put: %v", err)
	}

	d, ok, err := st.GetDomain(ctx, "app.example.com")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if d.Host != "app.example.com" || d.Status != "pending" || d.Port != 8000 || d.Wildcard {
		t.Fatalf("row = %+v", d)
	}

	// Another org may not take the host, and the loser's write changes nothing.
	if err := st.PutDomain(ctx, Domain{Host: "app.example.com", OrgID: "org2", SandboxID: "sb-9", Port: 3000}); !errors.Is(err, ErrDomainTaken) {
		t.Fatalf("second org: err=%v, want ErrDomainTaken", err)
	}
	if d, _, _ := st.GetDomain(ctx, "app.example.com"); d.OrgID != "org1" || d.SandboxID != "sb-1" {
		t.Fatalf("after refused put the row moved: %+v", d)
	}

	// Verification result, then the 10-minute re-check pass finds it again.
	if err := st.SetDomainStatus(ctx, "app.example.com", "active", "", 100); err != nil {
		t.Fatalf("set status: %v", err)
	}
	if d, _, _ := st.GetDomain(ctx, "app.example.com"); d.Status != "active" || d.CheckedAt != 100 {
		t.Fatalf("after activate: %+v", d)
	}
	if due, err := st.DomainsDue(ctx, 50, 10); err != nil || len(due) != 0 {
		t.Fatalf("fresh active row is due: %v %v", due, err)
	}
	if due, err := st.DomainsDue(ctx, 200, 10); err != nil || len(due) != 1 {
		t.Fatalf("stale active row not due: %v %v", due, err)
	}

	// Another org's delete misses; the owner's removes the row.
	if ok, err := st.DeleteDomain(ctx, "org2", "app.example.com"); err != nil || ok {
		t.Fatalf("cross-org delete: ok=%v err=%v", ok, err)
	}

	// Destroying the sandbox takes its hosts with it.
	if err := st.DeleteSandbox(ctx, "sb-1"); err != nil {
		t.Fatalf("delete sandbox: %v", err)
	}
	if _, ok, err := st.GetDomain(ctx, "app.example.com"); err != nil || ok {
		t.Fatalf("host survived the sandbox: ok=%v err=%v", ok, err)
	}
}

func TestDomainWildcardListing(t *testing.T) {
	st := openTemp(t)
	ctx := t.Context()

	if err := st.PutDomain(ctx, Domain{Host: "*.vms.example.com", OrgID: "org1", Wildcard: true}); err != nil {
		t.Fatalf("put wildcard: %v", err)
	}
	if err := st.PutDomain(ctx, Domain{Host: "one.example.com", OrgID: "org1", SandboxID: "sb-1", Port: 8000}); err != nil {
		t.Fatalf("put exact: %v", err)
	}
	// Pending wildcards must not route, so the ingress listing excludes them.
	if got, err := st.ActiveWildcardDomains(ctx); err != nil || len(got) != 0 {
		t.Fatalf("pending wildcard listed: %v %v", got, err)
	}
	if err := st.SetDomainStatus(ctx, "*.vms.example.com", "active", "", 5); err != nil {
		t.Fatalf("activate: %v", err)
	}
	got, err := st.ActiveWildcardDomains(ctx)
	if err != nil || len(got) != 1 || got[0].Host != "*.vms.example.com" || !got[0].Wildcard {
		t.Fatalf("wildcards = %+v err=%v", got, err)
	}
	if list, err := st.ListDomainsByOrg(ctx, "org1"); err != nil || len(list) != 2 {
		t.Fatalf("org listing = %+v err=%v", list, err)
	}
	if list, err := st.ListDomainsByOrg(ctx, "org2"); err != nil || len(list) != 0 {
		t.Fatalf("other org sees %+v err=%v", list, err)
	}
}

func TestOrgPreviewSourcesRoundTrip(t *testing.T) {
	st := openTemp(t)
	ctx := t.Context()

	if got, err := st.OrgPreviewSources(ctx, "org1"); err != nil || len(got) != 0 {
		t.Fatalf("unset: %v %v", got, err)
	}
	if err := st.SetOrgPreviewSources(ctx, "org1", []string{"100.64.0.0/10", "10.0.0.0/8"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := st.OrgPreviewSources(ctx, "org1")
	if err != nil || len(got) != 2 || got[0] != "100.64.0.0/10" {
		t.Fatalf("round trip = %v err=%v", got, err)
	}
	if err := st.SetOrgPreviewSources(ctx, "org1", []string{"garbage"}); err == nil {
		t.Fatal("a non-CIDR was stored")
	}
	if got, _ := st.OrgPreviewSources(ctx, "org1"); len(got) != 2 {
		t.Fatalf("refused write changed the row: %v", got)
	}
	if err := st.SetOrgPreviewSources(ctx, "org1", nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got, _ := st.OrgPreviewSources(ctx, "org1"); len(got) != 0 {
		t.Fatalf("after clear: %v", got)
	}
}

// Claiming a host proves nothing — the CNAME is the proof and the verifier is
// what checks it — so a claim that never verifies must not hold the name for
// ever. One tenant could otherwise reserve a competitor's hostname in a shared
// deployment by typing it in once.
func TestUnverifiedDomainClaimExpires(t *testing.T) {
	st := openTemp(t)
	ctx := t.Context()
	const day = int64(24 * 60 * 60)

	if err := st.PutDomain(ctx, Domain{Host: "rival.example.com", OrgID: "squatter", CreatedAt: day}); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	// Inside the grace period the claim holds.
	if err := st.PutDomain(ctx, Domain{Host: "rival.example.com", OrgID: "rival", CreatedAt: day + 6*day}); !errors.Is(err, ErrDomainTaken) {
		t.Fatalf("claim at 6 days: err=%v, want ErrDomainTaken", err)
	}
	// Re-pointing your own row must not buy another week.
	if err := st.PutDomain(ctx, Domain{Host: "rival.example.com", OrgID: "squatter", CreatedAt: day + 6*day}); err != nil {
		t.Fatalf("own re-point: %v", err)
	}
	// Past it, the real owner may claim the name, and the row is theirs.
	if err := st.PutDomain(ctx, Domain{Host: "rival.example.com", OrgID: "rival", CreatedAt: day + DomainClaimGrace + 1}); err != nil {
		t.Fatalf("claim after the grace period: %v, want it to succeed", err)
	}
	d, _, _ := st.GetDomain(ctx, "rival.example.com")
	if d.OrgID != "rival" || d.Status != "pending" {
		t.Fatalf("after takeover the row is %+v, want org rival and pending", d)
	}

	// A VERIFIED row is not takeable, however old: the host is routing for the
	// org that proved it, and age is not evidence against them.
	if err := st.SetDomainStatus(ctx, "rival.example.com", "active", "", day); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if err := st.PutDomain(ctx, Domain{Host: "rival.example.com", OrgID: "squatter", CreatedAt: 10 * DomainClaimGrace}); !errors.Is(err, ErrDomainTaken) {
		t.Fatalf("claim against an active row: err=%v, want ErrDomainTaken", err)
	}
	if d, _, _ := st.GetDomain(ctx, "rival.example.com"); d.OrgID != "rival" || d.Status != "active" {
		t.Fatalf("an active row moved or was reset: %+v", d)
	}
}

// The TXT token is what a customer's record holds, so once a row has one it
// never changes for that org. A new holder gets a fresh token and no clock.
func TestDomainTXTTokenIsMintedOnceAndKept(t *testing.T) {
	st, ctx := openTemp(t), t.Context()
	get := func() Domain {
		t.Helper()
		d, ok, err := st.GetDomain(ctx, "app.example.com")
		if err != nil || !ok {
			t.Fatalf("get: ok=%v err=%v", ok, err)
		}
		return d
	}

	if err := st.PutDomain(ctx, Domain{Host: "app.example.com", OrgID: "org1", CreatedAt: 10}); err != nil {
		t.Fatalf("put: %v", err)
	}
	token := get().TXTToken
	if len(token) < 26 {
		t.Fatalf("token %q on insert, want at least 128 random bits", token)
	}
	if err := st.SetDomainTXT(ctx, "app.example.com", "other", 500); err != nil {
		t.Fatalf("set txt: %v", err)
	}
	if d := get(); d.TXTToken != token || d.TXTRequiredAt != 500 {
		t.Fatalf("row = %+v, want token %q kept and the TXT required at 500", d, token)
	}
	if err := st.PutDomain(ctx, Domain{Host: "app.example.com", OrgID: "org1", Port: 9000, CreatedAt: 20}); err != nil {
		t.Fatalf("re-point: %v", err)
	}
	if d := get(); d.TXTToken != token || d.TXTRequiredAt != 500 {
		t.Fatalf("after re-pointing, row = %+v, want token and clock kept", d)
	}

	// A row from before tokens existed gets one from the verifier, once.
	if _, err := st.exec(ctx, `UPDATE custom_domains SET txt_token = '', txt_required_at = 0`); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if err := st.SetDomainTXT(ctx, "App.Example.com", "first", 600); err != nil {
		t.Fatalf("set txt: %v", err)
	}
	if err := st.SetDomainTXT(ctx, "app.example.com", "second", 0); err != nil {
		t.Fatalf("set txt: %v", err)
	}
	if d := get(); d.TXTToken != "first" || d.TXTRequiredAt != 0 {
		t.Fatalf("row = %+v, want the first token kept and the clock cleared", d)
	}

	if err := st.PutDomain(ctx, Domain{Host: "app.example.com", OrgID: "org2", CreatedAt: 10 + DomainClaimGrace + 1}); err != nil {
		t.Fatalf("takeover: %v", err)
	}
	if d := get(); d.TXTToken == "first" || len(d.TXTToken) < 26 || d.TXTRequiredAt != 0 {
		t.Fatalf("after another org took the host, row = %+v, want a new token and no clock", d)
	}

	var name string
	if err := st.queryRow(ctx, `SELECT name FROM schema_migrations WHERE version = 75`).Scan(&name); err != nil || name != "custom_domain_txt" {
		t.Fatalf("v75 not recorded (name=%q err=%v)", name, err)
	}
}
