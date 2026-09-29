package daemon

import (
	"github.com/pilldrops-oss/pilldrops/internal/auth/sbtoken"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// identityFor mints a sandbox's own platform credential. The raw token is
// handed to the runtime for delivery into the guest and dropped; only the hash
// is kept, on the sandbox row, which destroy deletes. Off (zero, "") when the
// node has no API URL to hand the guest: a token that points nowhere is a
// credential with no use and a row that reads as if it had one.
func (d *Daemon) identityFor(id string) (rt.Identity, string) {
	if d.cfg.IdentityAPIURL == "" {
		return rt.Identity{}, ""
	}
	tok, _ := sbtoken.Mint()
	tok = sbtoken.IdentityPrefix + tok
	return rt.Identity{
		Token: tok, APIURL: d.cfg.IdentityAPIURL, SandboxID: id, Domain: d.cfg.SandboxDomain,
	}, sbtoken.HashHex(tok)
}
