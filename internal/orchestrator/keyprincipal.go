package orchestrator

// The key below the org. A customer key names itself; the portal, which holds
// the operator credential, names the key it authenticated in X-Key-Id beside
// the org it asserts in X-Org-Override. Both are relayed to the node inside
// the signed assertion, so a tenant cannot claim to be another key any more
// than it can claim another org.

import (
	"context"
	"net/http"
)

const (
	// keyIDHeader and keyRoleHeader name the calling key. Honoured from the
	// operator credential only, and never in relayClientHeaders: a customer
	// asserting its own key id would be forging attribution.
	keyIDHeader   = "X-Key-Id"
	keyRoleHeader = "X-Key-Role"
)

const (
	ctxKeyID   ctxKey = 101
	ctxKeyRole ctxKey = 102
)

func withKey(ctx context.Context, id, role string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(context.WithValue(ctx, ctxKeyID, id), ctxKeyRole, role)
}

// callerKey returns the key this request acts as and its role; "" for a
// request no key id reached (an unnamed hop, an operator acting as itself).
// A sandbox principal is named: authSandbox stamps sandbox.SelfKeyID here, and
// the ownership rules recognise it. A sandbox created by an unnamed caller
// belongs to nobody, which the visibility rule reads as org-visible: the
// behaviour before keys had ids at all.
func callerKey(ctx context.Context) (id, role string) {
	id, _ = ctx.Value(ctxKeyID).(string)
	role, _ = ctx.Value(ctxKeyRole).(string)
	return id, role
}

// keyPrincipalFrom reads the portal's assertion of the calling key. Only for
// the operator branch of auth(); a customer key names itself from its own row.
func keyPrincipalFrom(r *http.Request) (id, role string) {
	return r.Header.Get(keyIDHeader), r.Header.Get(keyRoleHeader)
}
