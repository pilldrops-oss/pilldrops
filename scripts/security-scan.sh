#!/usr/bin/env bash
# security-scan.sh: govulncheck, gosec, staticcheck and osv-scanner, failing
# only on findings not in scripts/security-baseline.txt. Keys are tool|rule|file
# with no line number, so the baseline survives edits above a finding.
#
# Exit codes:
#   0  no new findings
#   1  new findings (or a stale baseline entry, with --strict)
#   2  prerequisites unavailable; not a pass
#
# Usage:
#   scripts/security-scan.sh                  # all four tools
#   scripts/security-scan.sh gosec osv        # a subset
#   scripts/security-scan.sh --update         # rewrite the baseline
#   scripts/security-scan.sh --strict         # also fail on stale entries
set -uo pipefail

cd "$(dirname "$0")/.." || exit 2

BASELINE="scripts/security-baseline.txt"

# Pinned so the analyser cannot change under the gate; advisory data is still fetched fresh.
GOSEC_VERSION="v2.22.9"
STATICCHECK_VERSION="2025.1.1"
GOVULNCHECK_VERSION="v1.1.4"
OSVSCANNER_VERSION="v1.9.2"

UPDATE=0
STRICT=0
TOOLS=()
for arg in "$@"; do
  case "$arg" in
    --update) UPDATE=1 ;;
    --strict) STRICT=1 ;;
    -*) echo "unknown flag: $arg" >&2; exit 2 ;;
    *) TOOLS+=("$arg") ;;
  esac
done
[ "${#TOOLS[@]}" -gt 0 ] || TOOLS=(govulncheck gosec staticcheck osv)

command -v go >/dev/null || { echo "no Go toolchain" >&2; exit 2; }

# gosec and staticcheck only see files the current GOOS selects, and internal/vm
# is //go:build linux, so a baseline generated elsewhere under-covers it.
if [ "$UPDATE" = "1" ] && [ "$(go env GOOS)" != "linux" ]; then
  cat >&2 <<'WHY'
refusing to regenerate the baseline on GOOS=darwin.

gosec and staticcheck only see files the current GOOS compiles, and every file
in internal/vm is //go:build linux. A baseline written here would omit the VM
lifecycle, the jailer, overlay sizing and the storage cap entirely — and the
next Linux run would report all of them as NEW.

Regenerate where CI runs:

  docker run --rm -v "$PWD":/src -w /src -e HOME=/tmp golang:1.25 \
    bash -c './scripts/security-scan.sh --update'
WHY
  exit 2
fi
GOBIN="$(go env GOPATH)/bin"
PATH="$GOBIN:$PATH"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
FINDINGS="$WORK/findings"
: >"$FINDINGS"
DETAIL="$WORK/detail"
: >"$DETAIL"

# ensure <binary> <module@version>
ensure() {
  command -v "$1" >/dev/null && return 0
  echo "installing $2 …" >&2
  go install "$2" >&2 || { echo "could not install $2" >&2; return 1; }
  command -v "$1" >/dev/null
}

ran_any=0

# govulncheck is call-graph aware (reachable symbols only); osv-scanner is
# module-graph level. They complement each other.
run_govulncheck() {
  ensure govulncheck "golang.org/x/vuln/cmd/govulncheck@${GOVULNCHECK_VERSION}" || return 2
  govulncheck -json ./... >"$WORK/govulncheck.json" 2>"$WORK/govulncheck.err"
  rc=$?
  # Non-zero exit means findings; only no output at all is a prerequisite problem.
  if [ ! -s "$WORK/govulncheck.json" ]; then
    sed 's/^/  /' "$WORK/govulncheck.err" >&2
    return 2
  fi
  python3 - "$WORK/govulncheck.json" >>"$FINDINGS" 2>>"$DETAIL" <<'PY'
import json, sys
# govulncheck -json emits a stream of single-key JSON objects.
buf = open(sys.argv[1]).read()
dec = json.JSONDecoder()
i, osvs, seen = 0, {}, set()
while i < len(buf):
    while i < len(buf) and buf[i].isspace():
        i += 1
    if i >= len(buf):
        break
    obj, i = dec.raw_decode(buf, i)
    if "osv" in obj:
        osvs[obj["osv"]["id"]] = obj["osv"]
    f = obj.get("finding")
    # Only called findings: the innermost trace frame carries a "function" only
    # when there is a real call path. Import-level rows are osv-scanner's job.
    if f and f.get("trace") and f.get("osv") and f["trace"][0].get("function"):
        key = (f["osv"], f.get("trace")[0].get("module", "?"))
        if key in seen:
            continue
        seen.add(key)
        print("govulncheck|%s|%s" % key)
        s = osvs.get(f["osv"], {}).get("summary", "")
        print("  govulncheck %s in %s: %s" % (key[0], key[1], s), file=sys.stderr)
PY
  return 0
}

run_gosec() {
  ensure gosec "github.com/securego/gosec/v2/cmd/gosec@${GOSEC_VERSION}" || return 2
  # gosec walks the filesystem, not the module graph, so exclude the worktrees under .claude.
  gosec -quiet -exclude-dir=vendor -exclude-dir=.claude -fmt=json -out "$WORK/gosec.json" ./... >/dev/null 2>"$WORK/gosec.err"
  if [ ! -s "$WORK/gosec.json" ]; then
    sed 's/^/  /' "$WORK/gosec.err" >&2
    return 2
  fi
  python3 - "$WORK/gosec.json" >>"$FINDINGS" 2>>"$DETAIL" <<'PY'
import json, sys, os
d = json.load(open(sys.argv[1]))
root = os.getcwd() + "/"
for i in d.get("Issues", []):
    f = i.get("file", "")
    if f.startswith(root):
        f = f[len(root):]
    print("gosec|%s|%s" % (i.get("rule_id", "?"), f))
    print("  gosec %s %s %s:%s %s" % (i.get("rule_id"), i.get("severity"),
          f, i.get("line"), i.get("details", "")[:90]), file=sys.stderr)
PY
  return 0
}

# Overlaps golangci-lint's staticcheck, but the two disagree on which non-SA checks are on.
run_staticcheck() {
  ensure staticcheck "honnef.co/go/tools/cmd/staticcheck@${STATICCHECK_VERSION}" || return 2
  staticcheck -f json ./... >"$WORK/staticcheck.json" 2>"$WORK/staticcheck.err"
  if [ ! -s "$WORK/staticcheck.json" ] && [ -s "$WORK/staticcheck.err" ]; then
    sed 's/^/  /' "$WORK/staticcheck.err" >&2
    return 2
  fi
  python3 - "$WORK/staticcheck.json" >>"$FINDINGS" 2>>"$DETAIL" <<'PY'
import json, sys, os
root = os.getcwd() + "/"
for line in open(sys.argv[1]):
    line = line.strip()
    if not line:
        continue
    o = json.loads(line)
    if o.get("severity") == "ignored":
        continue
    f = o.get("location", {}).get("file", "")
    if f.startswith(root):
        f = f[len(root):]
    print("staticcheck|%s|%s" % (o.get("code", "?"), f))
    print("  staticcheck %s %s:%s %s" % (o.get("code"), f,
          o.get("location", {}).get("line"), o.get("message", "")[:90]), file=sys.stderr)
PY
  return 0
}

run_osv() {
  ensure osv-scanner "github.com/google/osv-scanner/cmd/osv-scanner@${OSVSCANNER_VERSION}" || return 2
  osv-scanner --format json --lockfile go.mod >"$WORK/osv.json" 2>"$WORK/osv.err"
  if [ ! -s "$WORK/osv.json" ]; then
    sed 's/^/  /' "$WORK/osv.err" >&2
    return 2
  fi
  python3 - "$WORK/osv.json" >>"$FINDINGS" 2>>"$DETAIL" <<'PY'
import json, sys
# osv-scanner may print warnings on stdout ahead of the JSON; start at the first brace.
raw = open(sys.argv[1]).read()
i = raw.find("{")
if i < 0:
    sys.exit("osv-scanner produced no JSON")
d = json.loads(raw[i:])
for r in d.get("results", []):
    for p in r.get("packages", []):
        name = p["package"]["name"]
        ver = p["package"].get("version", "?")
        for v in p.get("vulnerabilities", []):
            print("osv|%s|%s" % (v["id"], name))
            print("  osv %s %s@%s %s" % (v["id"], name, ver,
                  (v.get("summary") or "")[:90]), file=sys.stderr)
PY
  return 0
}

for t in "${TOOLS[@]}"; do
  case "$t" in
    govulncheck) run_govulncheck ;;
    gosec)       run_gosec ;;
    staticcheck) run_staticcheck ;;
    osv|osv-scanner) run_osv ;;
    *) echo "unknown tool: $t" >&2; exit 2 ;;
  esac
  rc=$?
  if [ "$rc" -eq 2 ]; then
    echo "PREREQ: $t could not run on this machine" >&2
    exit 2
  fi
  ran_any=1
done

[ "$ran_any" -eq 1 ] || { echo "no scanner ran" >&2; exit 2; }

sort -u "$FINDINGS" -o "$FINDINGS"

if [ "$UPDATE" -eq 1 ]; then
  {
    cat <<'HEADER'
# scripts/security-baseline.txt — findings that are KNOWN, not findings that
# are fine. Generated by scripts/security-scan.sh --update; do not hand-edit the
# comment block, it is regenerated from the script.
#
# GENERATED ON LINUX, deliberately. gosec and staticcheck analyse only what the
# current GOOS compiles, and all of internal/vm is //go:build linux — a baseline
# made on macOS silently omits the VM lifecycle, the jailer, overlay sizing and
# the storage cap. That is not hypothetical: the macOS-generated file held 102
# keys, the Linux one holds 136. `--update` now refuses off Linux.
#
# Format: <tool>|<rule or advisory id>|<file or module>
# Line numbers are deliberately absent — a key that moves whenever anyone edits
# above it produces a baseline that gets regenerated without being read.
#
# What the initial sweep found, and why it is baselined rather than fixed
# ----------------------------------------------------------------------
# gosec, 93 keys. The HIGH ones were read individually:
#   G101 "hardcoded credentials" ×5 — all false: a SQL column list, an HTTP
#        header NAME, two error-code constants, and a default filename.
#   G404 "weak random" — retry-backoff jitter. Nothing that mints a
#        credential uses math/rand; sbtoken and the sandbox-id generator use
#        crypto/rand.
#   G115 "integer overflow on conversion" ×23 — the noisiest rule in gosec on
#        any Go codebase; kept in the baseline rather than excluded so a
#        conversion in a NEW file still surfaces.
#   G301/G302/G306 (permissions), G304 (file path from a variable) and G204
#        (subprocess from a variable) are the shape of this program: it makes
#        overlay files, opens tenant-named paths and runs the jailer.
#        internal/vm has its own path-confinement tests for the ones that
#        matter (3.6, 4.5).
#
# osv-scanner, 7 keys, 3 modules: logrus 1.8.1 (DoS via Entry.Writer),
#   mongo-driver 1.8.3 (OOB read in GSSAPI error handling) and chi 5.2.5
#   (three IP-spoofing advisories in middleware.RealIP). All three are
#   transitively required, none is called: `grep -r RealIP` over non-vendor Go
#   returns nothing, and govulncheck — which is call-graph aware — reports
#   "0 vulnerabilities called". They stay listed because osv-scanner's job is
#   to say "you are one call away", and that is worth knowing.
#
# govulncheck, 0 keys. Only CALLED findings are recorded here; that is the
#   whole reason it and osv-scanner are both wired.
#
# staticcheck, 2 keys, both cosmetic, both outside this change's scope.
#
# Adding a line is a decision. Say why, above it, in a comment.
HEADER
    echo
    cat "$FINDINGS"
  } >"$BASELINE"
  echo "wrote $BASELINE ($(wc -l <"$FINDINGS" | tr -d ' ') findings)"
  exit 0
fi

if [ ! -f "$BASELINE" ]; then
  echo "FAIL: $BASELINE does not exist. Create it with: $0 --update" >&2
  exit 1
fi

grep -v '^\s*#' "$BASELINE" | grep -v '^\s*$' | sort -u >"$WORK/baseline"

NEW="$(comm -23 "$FINDINGS" "$WORK/baseline")"
GONE="$(comm -13 "$FINDINGS" "$WORK/baseline")"

# A subset run cannot judge the other tools' baseline lines as stale.
if [ "${#TOOLS[@]}" -lt 4 ]; then
  GONE=""
fi

if [ -n "$NEW" ]; then
  echo "FAIL: new security findings, not in $BASELINE:"
  echo
  while IFS= read -r k; do
    [ -n "$k" ] || continue
    echo "  $k"
    # The key has no line number; the detail line does.
    rule=$(printf '%s' "$k" | cut -d'|' -f2)
    where=$(printf '%s' "$k" | cut -d'|' -f3)
    grep -F -- "$rule" "$DETAIL" | grep -F -- "$where" | sed 's/^/     /' | head -5
  done <<<"$NEW"
  cat <<EOF

Fix it, or — if it is a false positive or an accepted risk — add the key to
$BASELINE with a comment saying why. Regenerating the whole file with --update
without reading the diff defeats the point of having one.
EOF
  exit 1
fi

if [ -n "$GONE" ]; then
  echo "NOTE: $(printf '%s\n' "$GONE" | grep -c .) baseline entr(y|ies) no longer reported — the finding was fixed:"
  printf '%s\n' "$GONE" | sed 's/^/  /'
  echo "Refresh with: $0 --update"
  if [ "$STRICT" -eq 1 ]; then
    echo "(--strict: a stale baseline is a failure)"
    exit 1
  fi
fi

echo "OK: $(wc -l <"$FINDINGS" | tr -d ' ') finding(s), all baselined; 0 new. Tools: ${TOOLS[*]}"
