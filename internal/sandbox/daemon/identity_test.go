package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/auth/sbtoken"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/runtimetest"
)

// Every create hands the runtime a fresh pd_sb_ token for delivery and keeps
// only its hash; destroy drops the row; a fork's child gets its own.
func TestIdentityMintedPerSandboxAndRevokedOnDestroy(t *testing.T) {
	fake := runtimetest.NewFake()
	p := newFakePersister(nil)
	d := New(Config{
		Runtime: fake, Store: p, IdleTimeout: time.Minute, HardTTL: time.Hour,
		IdentityAPIURL: "https://api.example.test", SandboxDomain: "sb.example.test",
	})
	ctx := context.Background()

	parent, err := d.Create(ctx, CreateSpec{Template: "base", OrgID: "org-a"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	spec, _ := fake.LastBootSpec()
	if !strings.HasPrefix(spec.Identity.Token, sbtoken.IdentityPrefix) || spec.Identity.SandboxID != parent.ID ||
		spec.Identity.APIURL != "https://api.example.test" || spec.Identity.Domain != "sb.example.test" {
		t.Fatalf("runtime was handed %+v", spec.Identity)
	}
	parentHash := sbtoken.HashHex(spec.Identity.Token)
	if got := p.saved[parent.ID].IdentityHash; got != parentHash {
		t.Fatalf("persisted hash %q, want sha256 of the delivered token %q", got, parentHash)
	}

	child, err := d.Fork(ctx, parent.ID, ForkSpec{})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	cspec, _ := fake.LastBootSpec()
	if cspec.Identity.Token == spec.Identity.Token || cspec.Identity.SandboxID != child.Child.ID {
		t.Fatalf("child identity %+v reuses the parent's or names the wrong sandbox", cspec.Identity)
	}
	childHash := p.saved[child.Child.ID].IdentityHash
	if childHash == "" || childHash == parentHash || childHash != sbtoken.HashHex(cspec.Identity.Token) {
		t.Fatalf("child hash %q parent hash %q", childHash, parentHash)
	}
	// The raw token is never persisted anywhere the daemon can see.
	for id, rec := range p.saved {
		if strings.Contains(rec.IdentityHash, sbtoken.IdentityPrefix) {
			t.Fatalf("%s persisted a raw token", id)
		}
	}

	if err := d.Destroy(ctx, parent.ID); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if !p.deleted[parent.ID] || p.deleted[child.Child.ID] {
		t.Fatalf("destroy must delete exactly the parent's row: deleted=%v", p.deleted)
	}
}

// No API URL means no identity: nothing minted, nothing pushed, no hash on the row.
func TestIdentityOffWithoutAPIURL(t *testing.T) {
	d, fake := newTestDaemon(t)
	sb, err := d.Create(context.Background(), CreateSpec{Template: "base", OrgID: "org-a"})
	if err != nil {
		t.Fatal(err)
	}
	if spec, _ := fake.LastBootSpec(); spec.Identity != (rt.Identity{}) {
		t.Fatalf("identity handed to the runtime with no API URL configured: %+v", spec.Identity)
	}
	if d.boxes[sb.ID].IdentityHash != "" {
		t.Fatal("a hash was recorded for a token that was never delivered")
	}
}
