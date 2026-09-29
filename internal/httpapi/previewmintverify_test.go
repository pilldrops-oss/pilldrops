package httpapi

import (
	"context"
	"crypto/ed25519"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
)

// The minter and the verifier of ?ptok= live in one file and read one fact
// about which secret signs the way in. This is the check that fails if they
// stop agreeing: whatever appURL puts in a link, this node's own gate must
// open — or the link must carry no credential at all, which says "ask the
// control plane in front of me". Both halves were covered before and both
// passed; between them every fleet node returned a URL it would refuse.
func TestEveryTokenAppURLEmbedsIsOneThisNodeAccepts(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	for _, tc := range []struct {
		name      string
		orgAssert ed25519.PublicKey
		domain    string
		wantTok   bool
	}{
		{"standalone node, path form", nil, "", true},
		{"standalone node, subdomain form", nil, "sb.test", true},
		{"fleet node, path form", pub, "", false},
		{"fleet node, subdomain form", pub, "sb.test", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, st := newDaemonForPreviewTest(t)
			s := New(d, st)
			s.SetPreviewSecret([]byte(testPreviewSecret))
			s.SetOrgAssertPublicKey(tc.orgAssert)
			s.SetSandboxDomain(tc.domain)
			sb, err := d.Create(context.Background(), daemon.CreateSpec{Template: "base", OrgID: "u:cust1"})
			if err != nil {
				t.Fatalf("create sandbox: %v", err)
			}

			r := httptest.NewRequest("POST", "http://node.test/v1/sandboxes", nil)
			link := s.appURL(r, sb.ID, 8000, sb.OrgID)
			u, err := url.Parse(link)
			if err != nil {
				t.Fatalf("appURL returned an unparseable link %q: %v", link, err)
			}
			tok := u.Query().Get("ptok")

			if tok != "" {
				if verdict, org := s.checkPreviewAuth(sb.ID, 8000, tok); verdict != previewAuthOK || org != sb.OrgID {
					t.Fatalf("this node refused the token it just minted: appURL gave %q, "+
						"checkPreviewAuth answered verdict=%d org=%q", link, verdict, org)
				}
			}
			// The positive control, so "mints nothing" cannot pass this test
			// everywhere: a standalone node is the only minter there is and
			// must still embed one.
			if (tok != "") != tc.wantTok {
				t.Fatalf("link %q carries ptok=%q; want a token: %v", link, tok, tc.wantTok)
			}
			if tok == "" {
				return
			}
			// The gate is not simply open: a link from a different port must
			// still be refused, or the check above proves nothing.
			if verdict, _ := s.checkPreviewAuth(sb.ID, 9000, tok); verdict == previewAuthOK {
				t.Error("the :8000 token also opened :9000 — this gate accepts anything")
			}
		})
	}
}
