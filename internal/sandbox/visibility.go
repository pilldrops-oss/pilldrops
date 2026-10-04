package sandbox

import (
	"fmt"
	"strconv"
)

// Visibility values. A sandbox is org-visible unless its creator says
// otherwise, which is what every sandbox was before visibility existed: an
// empty column reads as VisibilityOrg, so no migrated row is ever hidden.
const (
	VisibilityOrg     = "org"
	VisibilityPrivate = "private"
)

// ValidVisibility normalises a requested visibility. "" is the default, "org".
func ValidVisibility(v string) (string, error) {
	switch v {
	case "":
		return VisibilityOrg, nil
	case VisibilityOrg, VisibilityPrivate:
		return v, nil
	default:
		return "", fmt.Errorf("visibility must be %q or %q, got %q", VisibilityOrg, VisibilityPrivate, v)
	}
}

// SelfKeyPrefix marks a caller key that is a sandbox acting as itself.
const SelfKeyPrefix = "sb:"

// SelfKeyID is the caller key a sandbox acting as itself carries. The control
// plane stamps it on a pd_sb_ principal and signs it into the org assertion;
// the node reads it back from there, and Visible recognises it, so the places
// that have to agree on who a sandbox is read one definition.
func SelfKeyID(id string) string { return SelfKeyPrefix + id }

// Visible reports whether the key making a request may see the sandbox id in
// its own org. Org visibility (including the empty legacy value) is visible to
// every key; a private sandbox is its creator's, its own, and any operator's.
// The org match is the caller's to make: this decides only the principal below it.
//
// A sandbox is never hidden from itself. Its identity token is issued to one
// machine and names it, so refusing that machine its own row made a private
// sandbox unreachable by the very credential it was booted with — which forks
// felt first, since a fork child is private by default.
//
// A private sandbox with no recorded creator (an empty created_by) belongs to nobody
// and stays operator-only, so a caller whose own key id is empty — an agent
// token, an unsigned hop, a raw public port — never inherits it.
func Visible(id, visibility, createdBy, callerKeyID string, operator bool) bool {
	if visibility != VisibilityPrivate {
		return true
	}
	if operator {
		return true
	}
	if id != "" && callerKeyID == SelfKeyID(id) {
		return true
	}
	return CreatedByCaller(createdBy, callerKeyID)
}

// CreatedByCaller reports whether callerKeyID is the key recorded as a
// sandbox's creator. Visible's last clause, exported on its own because the
// visibility handler needs exactly it: a sandbox acting as itself may read its
// own row but must not flip its own visibility, so that handler cannot ask
// Visible, which admits it.
func CreatedByCaller(createdBy, callerKeyID string) bool {
	return createdBy != "" && createdBy == callerKeyID
}

// ReachableAnonymously reports whether a caller holding no credential at all
// may be let into the sandbox. A port forward publishes a raw TCP/UDP port:
// there is no request to authenticate and no token to ask for, so the row
// itself has to carry the permission. It is Visible asked for the only
// principal such a connection speaks for — nobody — which refuses a private
// sandbox and leaves an org-visible one published, as it has always been.
//
// # The asymmetry, recorded deliberately
//
// This asks `visibility`, which defaults to org, so it says yes for a
// default-created sandbox. The HTTP preview and the custom-domain path ask a
// different column, `public_preview`, an explicit opt-in defaulting to false
// (internal/orchestrator/preview.go, domains.go). The same default sandbox
// therefore refuses every visitor to its preview without a ?token= while its
// raw forwarded port admits the internet with no credential at all.
//
// That is intended, and it is boxd parity: creating a forward is a deliberate
// act naming one guest port, and a raw TCP port has nothing to authenticate —
// no header to read, no page to redirect, nowhere to put a token. A preview is
// a URL that can be pasted anywhere, so it asks for an opt-in. The two
// questions differ because the two doors differ.
//
// Do not "unify" them by making this ask public_preview: that would break
// every existing forward on a default sandbox, which is the normal case. If a
// deployment wants a forward to need the same opt-in, the honest change is a
// third column that says so, not a quiet reinterpretation of this one.
func ReachableAnonymously(visibility, createdBy string) bool {
	return Visible("", visibility, createdBy, "", false)
}

// PreviewGeneration is the revocation generation a browser preview token is
// bound to: the visibility, marked when the preview is public, and the count of
// access flips so far. The count is what retracts: every flip of visibility or
// public bumps it, so a token minted before a flip never validates again, even
// once the settings are flipped back. flips 0 renders exactly the generation
// used before the count existed, so tokens minted then keep working until the
// next flip. The minting portal and the verifying orchestrator both call this
// so they cannot drift.
func PreviewGeneration(visibility string, public bool, flips int64) string {
	if visibility == "" {
		visibility = VisibilityOrg
	}
	if public {
		visibility += "+public"
	}
	if flips > 0 {
		visibility += "#" + strconv.FormatInt(flips, 10)
	}
	return visibility
}
