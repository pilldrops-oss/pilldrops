#!/usr/bin/env bash
# Tier boundary guard: the control plane (cmd/orchestrator), node agents
# (cmd/sandboxd, cmd/sandboxd-fleet) and in-guest binaries (cmd/envd, agentd)
# must not link each other's packages. Checked against the link closure
# (`go list -deps`), since a transitive dependency ships just as surely.
#
# Usage: ./scripts/tier-boundary-guard.sh
set -euo pipefail

fail=0

# check <binary> <description> <forbidden-package-regex>
check() {
  local bin="$1" what="$2" pattern="$3" bad
  bad=$(CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go list -deps "./cmd/${bin}" \
        | grep -E "github.com/pilldrops-oss/pilldrops/internal/(${pattern})(/|\$)" || true)
  if [ -n "$bad" ]; then
    echo "::error::cmd/${bin} must not depend on ${what}:"
    echo "$bad" | sed 's/^/    /'
    fail=1
  else
    echo "  ok  cmd/${bin}: no ${what}"
  fi
}

# checktags <binary> <build-tags> <description> <forbidden-package-regex>
# The tags must reach `go list` itself or the closure reported is the default build's.
checktags() {
  local bin="$1" tags="$2" what="$3" pattern="$4" bad
  bad=$(CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go list -deps -tags "${tags}" "./cmd/${bin}" \
        | grep -E "github.com/pilldrops-oss/pilldrops/internal/(${pattern})(/|\$)" || true)
  if [ -n "$bad" ]; then
    echo "::error::cmd/${bin} built with -tags ${tags} must not depend on ${what}:"
    echo "$bad" | sed 's/^/    /'
    fail=1
  else
    echo "  ok  cmd/${bin} [-tags ${tags}]: no ${what}"
  fi
}

# checkpkg <internal-package> <description> <forbidden-package-regex>
# For boundaries one binary sits on both sides of.
checkpkg() {
  local pkg="$1" what="$2" pattern="$3" bad
  bad=$(CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go list -deps "./internal/${pkg}" \
        | grep -E "github.com/pilldrops-oss/pilldrops/internal/(${pattern})(/|\$)" || true)
  if [ -n "$bad" ]; then
    echo "::error::internal/${pkg} must not depend on ${what}:"
    echo "$bad" | sed 's/^/    /'
    fail=1
  else
    echo "  ok  internal/${pkg}: no ${what}"
  fi
}

# checkdirect <internal-package> <description> <forbidden-package-regex>
# Direct imports only, for packages that legitimately link something transitively.
checkdirect() {
  local pkg="$1" what="$2" pattern="$3" bad
  bad=$(CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go list -f '{{join .Imports "\n"}}' "./internal/${pkg}" \
        | grep -E "github.com/pilldrops-oss/pilldrops/internal/(${pattern})(/|\$)" || true)
  if [ -n "$bad" ]; then
    echo "::error::internal/${pkg} must not import ${what} directly:"
    echo "$bad" | sed 's/^/    /'
    fail=1
  else
    echo "  ok  internal/${pkg}: no direct import of ${what}"
  fi
}

# A worker node must never hold payment credentials, sign-in secrets or customer
# email. clerkauth, mailer and trials no longer exist but stay listed so a fork that
# adds them back is caught. extras/bridge/discord, not all of extras/bridge: Telegram is reachable from the node API.
CONTROL_PLANE='orchestrator|cluster|clerkauth|mailer|trials|extras/bridge/discord|dodopay|stripe'
for bin in sandboxd sandboxd-fleet; do
  check "$bin" "control-plane packages" "$CONTROL_PLANE"
done

# The orchestrator schedules and proxies; it never runs a sandbox. sandbox/daemon,
# not sandbox: internal/sandbox is policy the control plane legitimately links.
check orchestrator "node internals" \
  'sandbox/daemon|vm|httpapi|nodeboot|runtime/(docker|firecracker|process)'

# The control plane serves the customer dashboard (webui) but not the node's
# local operator pane, which is wired to a local daemon.
check orchestrator "the node-local operator dashboard" 'dashboard'

# cmd/sandboxd-fleet is the lean node agent: no embedded SPA, no operator dashboard.
check sandboxd-fleet "dashboard or embedded-UI packages" 'webui|dashboard'

# `-tags core` must produce a node that runs sandboxes and nothing else: no agent, chat or
# integration packages (aigw and mcpcat are gone but stay listed). auth/aat is absent on
# purpose: internal/store links it unconditionally, so the rule cannot pass yet.
checktags sandboxd-fleet core "the optional product packages" \
  'extras/agent|extras/bridge|extras/mcp|mcpcat|aigw'
# Optional features live under internal/extras. Only the two packages that wire
# routes and boot features may link them, and only from files tagged !core (the
# check above proves that). Every other internal package is engine.
extras_ok=1
for pkg in $(CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go list ./internal/... \
    | sed 's#github.com/pilldrops-oss/pilldrops/internal/##' \
    | grep -Ev '^(extras|httpapi|nodeboot)(/|$)'); do
  bad=$(CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go list -deps "./internal/${pkg}" \
        | grep -E 'github.com/pilldrops-oss/pilldrops/internal/extras(/|$)' || true)
  if [ -n "$bad" ]; then
    echo "::error::internal/${pkg} is engine and must not depend on internal/extras:"
    echo "$bad" | sed 's/^/    /'
    fail=1; extras_ok=0
  fi
done
[ "$extras_ok" -eq 1 ] && echo "  ok  engine packages: no internal/extras"

# cmd/envd ships inside a customer's sandbox; of internal/auth only sbtoken
# (a random token, hash-only) may cross that line.
for bin in envd; do
  check "$bin" "server-side auth packages" \
    'auth/(aat|adminauth|authcache|orgassert|previewtoken)'
done

# Anything the API needs from a driver goes through internal/runtime.
checkpkg httpapi "Firecracker VM internals" 'vm'

# nodeboot selects a runtime, so it links vm through firecracker regardless; it must not name it.
checkdirect nodeboot "Firecracker VM internals" 'vm'

# internal/templates is linked into every tier including the guest, so it
# must stay a leaf; the node-side artifact store reaches it via templates.SetSource.
checkpkg templates "artifact storage-plane packages" 'artifact'

if [ "$fail" -ne 0 ]; then
  echo
  echo "Tier boundary violated. If the dependency is genuinely correct, move the"
  echo "shared code into a package both tiers may import rather than widening"
  echo "this list — the list is the boundary."
  exit 1
fi
echo "Tier boundaries intact."
